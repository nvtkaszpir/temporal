package main

import (
	"flag"
	"io/ioutil"
	slog "log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/uber-common/bark"
	"github.com/uber/tchannel-go"
	"github.com/uber/tchannel-go/thrift"

	"code.uber.internal/go-common.git/x/config"
	"code.uber.internal/go-common.git/x/log"
	"code.uber.internal/go-common.git/x/metrics"
	jaeger "github.com/uber/jaeger-client-go/config"

	m "code.uber.internal/devexp/minions/.gen/go/minions"
	"code.uber.internal/devexp/minions/common"
	"code.uber.internal/devexp/minions/health/driver"
	"code.uber.internal/devexp/minions/persistence"
	"code.uber.internal/devexp/minions/workflow"
	wmetrics "code.uber.internal/devexp/minions/workflow/metrics"
	"code.uber.internal/go-common.git/x/tchannel"
	"github.com/Sirupsen/logrus"
	"github.com/pborman/uuid"
)

// Host is created for each host running the stress test
type (
	Host struct {
		hostName string
		engine   workflow.Engine
		reporter common.Reporter

		instancesWG            sync.WaitGroup
		doneCh                 chan struct{}
		config                 Configuration
		flowConfig             *workflowConfig
		runOnMinionsProduction bool
	}

	// Configuration is the configuration used by cherami-stress
	Configuration struct {
		TChannel xtchannel.Configuration
		Logging  log.Configuration
		Jaeger   jaeger.Configuration
		Metrics  metrics.Configuration `yaml:"metrics"`
	}
)

var generalMetrics = map[common.MetricName]common.MetricType{
	common.WorkflowsStartTotalCounter:      common.Counter,
	common.WorkflowsCompletionTotalCounter: common.Counter,
	common.ActivitiesTotalCounter:          common.Counter,
	common.DecisionsTotalCounter:           common.Counter,
	common.WorkflowEndToEndLatency:         common.Timer,
	common.ActivityEndToEndLatency:         common.Timer,
	common.DecisionsEndToEndLatency:        common.Timer,
}

// NewStressHost creates an instance of stress host
func newHost(engine workflow.Engine, instanceName string, reporter common.Reporter,
	runOnMinionsProduction bool, config Configuration, flowConfig *workflowConfig) *Host {
	h := &Host{
		engine:                 engine,
		hostName:               instanceName,
		reporter:               reporter,
		doneCh:                 make(chan struct{}),
		config:                 config,
		flowConfig:             flowConfig,
		runOnMinionsProduction: runOnMinionsProduction,
	}

	h.reporter.InitMetrics(generalMetrics)
	return h
}

// GetThriftClient gets thrift client.
func (s *Host) GetThriftClient(tchan *tchannel.Channel) m.TChanWorkflowService {
	tclient := thrift.NewClient(tchan, "uber-minions", nil)
	return m.NewTChanWorkflowServiceClient(tclient)
}

// Start is used the start the stress host
func (s *Host) Start() {
	launchCh := make(chan struct{})

	go func() {

		var service m.TChanWorkflowService

		if s.runOnMinionsProduction {
			// TChannel to production.
			tchan, err := s.config.TChannel.NewClient("demo-client", nil)
			if err != nil {
				log.Panicf("Failed to get a client for the uber-minions: %s\n", err.Error())
			}
			service = s.GetThriftClient(tchan)
		} else {
			serviceMockEngine := driver.NewServiceMockEngine(s.engine)
			serviceMockEngine.Start()
			service = serviceMockEngine
		}

		if s.flowConfig.replayWorkflow {
			replayWorkflow(service, s.reporter, s.flowConfig)
		} else {
			launchDemoWorkflow(service, s.reporter, s.flowConfig)
		}

		// close(launchCh)
	}()

	if _, ok := s.reporter.(*common.SimpleReporter); ok {
		go s.printMetric()
	}

	<-launchCh

	close(s.doneCh)
}

func (s *Host) printMetric() {
	sr, ok := s.reporter.(*common.SimpleReporter)
	if !ok {
		log.Error("Invalid reporter passed to printMetric.")
		return
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// sr.PrintStressMetric()
			// sr.PrintFinalMetric()
			if sr.IsProcessComplete() {
				// sr.PrintFinalMetric()
				return
			}

		case <-s.doneCh:
			return
		}
	}
}

func main() {
	var host string
	var emitMetric string
	var runActivities bool
	var runDeciders bool
	var startCount int
	var panicWorkflow bool
	var runGreeterActivity bool
	var runNameActivity bool
	var runSayGreetingActivity bool
	var runOnMinionsProduction bool
	var useWorkflowID string
	var useRunID string
	var replayWorkflow bool

	flag.StringVar(&host, "host", "127.0.0.1", "Cassandra host to use.")
	flag.BoolVar(&runActivities, "runActivities", false, "Run activites.")
	flag.BoolVar(&runDeciders, "runDeciders", false, "Run deciders.")
	flag.IntVar(&startCount, "startCount", 0, "start count workflows.")
	flag.StringVar(&emitMetric, "emitMetric", "local", "Metric source: m3 | local")
	flag.BoolVar(&panicWorkflow, "panicWorkflow", false, "To run panic workflow.")
	flag.BoolVar(&runGreeterActivity, "runGreeterActivity", false, "To run greeter activity.")
	flag.BoolVar(&runNameActivity, "runNameActivity", false, "To run name activity.")
	flag.BoolVar(&runSayGreetingActivity, "runSayGreetingActivity", false, "To run say greeting activity.")
	flag.BoolVar(&runOnMinionsProduction, "runOnMinionsProduction", false, "Run against the minions production")
	flag.StringVar(&useWorkflowID, "useWorkflowID", uuid.New(), "Use this as workflow ID")
	flag.StringVar(&useRunID, "useRunID", uuid.New(), "Use this as Run ID")
	flag.BoolVar(&replayWorkflow, "replayWorkflow", false, "replay Workflow")

	flag.Parse()

	// TODO: For demo disable go cql logging.
	slog.SetOutput(ioutil.Discard)

	// log.Info("Starting Demo.")
	var cfg Configuration
	if err := config.Load(&cfg); err != nil {
		log.WithField(common.TagErr, err).Fatal(`Error initializing configuration`)
	}
	log.Configure(&cfg.Logging, true)
	// log.Infof("Logging Level: %v", cfg.Logging.Level)

	if host == "" {
		ip, err := tchannel.ListenIP()
		if err != nil {
			log.WithField(common.TagErr, err).Fatal(`Failed to find local listen IP`)
		}

		host = ip.String()
	}

	instanceName, e := os.Hostname()
	if e != nil {
		log.WithField(common.TagErr, e).Fatal(`Error getting hostname`)
	}
	instanceName = strings.Replace(instanceName, "-", ".", -1)

	var reporter common.Reporter
	if emitMetric == "m3" {
		log.Infof("M3 metric reporter: hostport=%v, service=%v", cfg.Metrics.M3.HostPort, cfg.Metrics.M3.Service)
		m, e := cfg.Metrics.New()
		if e != nil {
			log.WithField(common.TagErr, e).Fatal(`Failed to initialize metrics`)
		}

		// create the common tags to be used by all services
		reporter = common.NewM3Reporter(m, map[string]string{
			common.TagHostname: instanceName,
		})
	} else {
		reporter = common.NewSimpleReporter(nil, nil)
	}

	m3ReporterClient := wmetrics.NewClient(reporter, wmetrics.Workflow)

	var engine workflow.Engine

	if !runOnMinionsProduction {
		if host == "127.0.0.1" {
			testBase := workflow.TestBase{}
			options := workflow.TestBaseOptions{}
			options.ClusterHost = host
			options.KeySpace = "workflow"
			options.DropKeySpace = false
			testBase.SetupWorkflowStoreWithOptions(options.TestBaseOptions)

			logrus.SetLevel(logrus.InfoLevel)

			engine = workflow.NewWorkflowEngine(testBase.WorkflowMgr, testBase.TaskMgr, bark.NewLoggerFromLogrus(logrus.New()))
		} else {
			executionPersistence, err2 := persistence.NewCassandraWorkflowExecutionPersistence(host, "workflow")
			if err2 != nil {
				panic(err2)
			}

			executionPersistenceClient := workflow.NewWorkflowExecutionPersistenceClient(executionPersistence, m3ReporterClient)

			taskPersistence, err3 := persistence.NewCassandraTaskPersistence(host, "workflow")
			if err3 != nil {
				panic(err3)
			}

			taskPersistenceClient := workflow.NewTaskPersistenceClient(taskPersistence, m3ReporterClient)

			engine = workflow.NewEngineWithMetricsImpl(
				workflow.NewWorkflowEngine(executionPersistenceClient, taskPersistenceClient, log.WithField("host", "workflow_host")),
				m3ReporterClient)
		}
	}

	config := &workflowConfig{runActivities: runActivities, runDeciders: runDeciders,
		startCount: startCount, panicWorkflow: panicWorkflow, runGreeterActivity: runGreeterActivity,
		runNameActivity: runNameActivity, runSayGreetingActivity: runSayGreetingActivity,
		useWorkflowID: useWorkflowID, useRunID: useRunID, replayWorkflow: replayWorkflow}
	h := newHost(engine, instanceName, reporter, runOnMinionsProduction, cfg, config)
	h.Start()
}
