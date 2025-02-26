package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "go.uber.org/automaxprocs"
	"golang.org/x/exp/slices"

	"github.com/facebookgo/inject"
	"github.com/facebookgo/startstop"
	libhoney "github.com/honeycombio/libhoney-go"
	"github.com/honeycombio/libhoney-go/transmission"
	"github.com/sirupsen/logrus"

	"github.com/honeycombio/refinery/app"
	"github.com/honeycombio/refinery/collect"
	"github.com/honeycombio/refinery/config"
	"github.com/honeycombio/refinery/internal/peer"
	"github.com/honeycombio/refinery/logger"
	"github.com/honeycombio/refinery/metrics"
	"github.com/honeycombio/refinery/sample"
	"github.com/honeycombio/refinery/service/debug"
	"github.com/honeycombio/refinery/sharder"
	"github.com/honeycombio/refinery/transmit"
)

// set by travis.
var BuildID string
var version string

type graphLogger struct {
}

// TODO: make this log properly
func (g graphLogger) Debugf(format string, v ...interface{}) {
	fmt.Printf(format, v...)
	fmt.Println()
}

func main() {
	opts, err := config.NewCmdEnvOptions(os.Args)
	if err != nil {
		fmt.Printf("Command line parsing error '%s' -- call with --help for usage.\n", err)
		os.Exit(1)
	}

	if BuildID == "" {
		version = "dev"
	} else {
		version = BuildID
	}

	if opts.Version {
		fmt.Println("Version: " + version)
		os.Exit(0)
	}

	if opts.InterfaceNames {
		ifaces, err := net.Interfaces()
		if err != nil {
			fmt.Printf("Error: %s\n", err)
			os.Exit(1)
		}
		// sort interfaces by name since there are a lot of them
		slices.SortFunc(ifaces, func(i, j net.Interface) bool {
			return i.Name < j.Name
		})
		for _, i := range ifaces {
			fmt.Println(i.Name)
		}
		os.Exit(0)
	}

	a := app.App{
		Version: version,
	}

	c, err := config.NewConfig(opts, func(err error) {
		if a.Logger != nil {
			a.Logger.Error().WithField("error", err).Logf("error loading config")
		}
	})
	if err != nil {
		fmt.Printf("%+v\n", err)
		os.Exit(1)
	}
	if opts.Validate {
		fmt.Println("Config and Rules validated successfully.")
		os.Exit(0)
	}
	c.RegisterReloadCallback(func() {
		if a.Logger != nil {
			a.Logger.Info().Logf("configuration change was detected and the configuration was reloaded")
		}
	})

	// get desired implementation for each dependency to inject
	lgr := logger.GetLoggerImplementation(c)
	collector := collect.GetCollectorImplementation(c)
	metricsSingleton := metrics.GetMetricsImplementation(c)
	shrdr := sharder.GetSharderImplementation(c)
	samplerFactory := &sample.SamplerFactory{}

	// set log level
	logLevel := c.GetLoggerLevel().String()
	if err := lgr.SetLevel(logLevel); err != nil {
		fmt.Printf("unable to set logging level: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.GetPeerTimeout())
	defer cancel()
	done := make(chan struct{})
	peers, err := peer.NewPeers(ctx, c, done)

	if err != nil {
		fmt.Printf("unable to load peers: %+v\n", err)
		os.Exit(1)
	}

	// upstreamTransport is the http transport used to send things on to Honeycomb
	upstreamTransport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial: (&net.Dialer{
			Timeout: 10 * time.Second,
		}).Dial,
		TLSHandshakeTimeout: 15 * time.Second,
	}

	// peerTransport is the http transport used to send things to a local peer
	peerTransport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial: (&net.Dialer{
			Timeout: 3 * time.Second,
		}).Dial,
		TLSHandshakeTimeout: 1200 * time.Millisecond,
	}

	genericMetricsRecorder := metrics.NewMetricsPrefixer("")
	upstreamMetricsRecorder := metrics.NewMetricsPrefixer("libhoney_upstream")
	peerMetricsRecorder := metrics.NewMetricsPrefixer("libhoney_peer")

	userAgentAddition := "refinery/" + version
	upstreamClient, err := libhoney.NewClient(libhoney.ClientConfig{
		Transmission: &transmission.Honeycomb{
			MaxBatchSize:          c.GetMaxBatchSize(),
			BatchTimeout:          c.GetBatchTimeout(),
			MaxConcurrentBatches:  libhoney.DefaultMaxConcurrentBatches,
			PendingWorkCapacity:   uint(c.GetUpstreamBufferSize()),
			UserAgentAddition:     userAgentAddition,
			Transport:             upstreamTransport,
			BlockOnSend:           true,
			EnableMsgpackEncoding: true,
			Metrics:               upstreamMetricsRecorder,
		},
	})
	if err != nil {
		fmt.Printf("unable to initialize upstream libhoney client")
		os.Exit(1)
	}

	peerClient, err := libhoney.NewClient(libhoney.ClientConfig{
		Transmission: &transmission.Honeycomb{
			MaxBatchSize:          c.GetMaxBatchSize(),
			BatchTimeout:          c.GetBatchTimeout(),
			MaxConcurrentBatches:  libhoney.DefaultMaxConcurrentBatches,
			PendingWorkCapacity:   uint(c.GetPeerBufferSize()),
			UserAgentAddition:     userAgentAddition,
			Transport:             peerTransport,
			DisableCompression:    !c.GetCompressPeerCommunication(),
			EnableMsgpackEncoding: true,
			Metrics:               peerMetricsRecorder,
		},
	})
	if err != nil {
		fmt.Printf("unable to initialize peer libhoney client")
		os.Exit(1)
	}

	stressRelief := &collect.StressRelief{Done: done}
	upstreamTransmission := transmit.NewDefaultTransmission(upstreamClient, upstreamMetricsRecorder, "upstream")
	peerTransmission := transmit.NewDefaultTransmission(peerClient, peerMetricsRecorder, "peer")

	var g inject.Graph
	if opts.Debug {
		g.Logger = graphLogger{}
	}
	objects := []*inject.Object{
		{Value: c},
		{Value: peers},
		{Value: lgr},
		{Value: upstreamTransport, Name: "upstreamTransport"},
		{Value: peerTransport, Name: "peerTransport"},
		{Value: upstreamTransmission, Name: "upstreamTransmission"},
		{Value: peerTransmission, Name: "peerTransmission"},
		{Value: shrdr},
		{Value: collector},
		{Value: metricsSingleton, Name: "metrics"},
		{Value: genericMetricsRecorder, Name: "genericMetrics"},
		{Value: upstreamMetricsRecorder, Name: "upstreamMetrics"},
		{Value: peerMetricsRecorder, Name: "peerMetrics"},
		{Value: version, Name: "version"},
		{Value: samplerFactory},
		{Value: stressRelief, Name: "stressRelief"},
		{Value: &a},
	}
	// we need to add the multimetrics children to the graph as well
	for _, obj := range metricsSingleton.Children() {
		objects = append(objects, &inject.Object{Value: obj})
	}
	err = g.Provide(objects...)
	if err != nil {
		fmt.Printf("failed to provide injection graph. error: %+v\n", err)
		os.Exit(1)
	}

	if opts.Debug {
		err = g.Provide(&inject.Object{Value: &debug.DebugService{Config: c}})
		if err != nil {
			fmt.Printf("failed to provide injection graph. error: %+v\n", err)
			os.Exit(1)
		}
	}

	if err := g.Populate(); err != nil {
		fmt.Printf("failed to populate injection graph. error: %+v\n", err)
		os.Exit(1)
	}

	// We need to remove the upstream and peer transmissions from the list of
	// objects to startstop. For reasons I haven't been able to figure out,
	// startstop sometimes decides to start them before the rest of the objects
	// are ready, which causes a panic. We have to do it this way because
	// startstop deliberately randomizes the graph order.
	var nonxmitObjects []*inject.Object
	for _, obj := range g.Objects() {
		if obj.Name == "upstreamTransmission" || obj.Name == "peerTransmission" {
			continue
		}
		nonxmitObjects = append(nonxmitObjects, obj)
	}

	// the logger provided to startstop must be valid before any service is
	// started, meaning it can't rely on injected configs. make a custom logger
	// just for this step
	ststLogger := logrus.New()
	// level, _ := logrus.ParseLevel(logLevel)
	ststLogger.SetLevel(logrus.DebugLevel)

	// we can stop all the objects in one call, but we need to start the
	// transmissions manually.
	defer startstop.Stop(g.Objects(), ststLogger)
	if err := startstop.Start(nonxmitObjects, ststLogger); err != nil {
		fmt.Printf("failed to start injected dependencies. error: %+v\n", err)
		os.Exit(1)
	}
	// now start these manually
	upstreamTransmission.Start()
	peerTransmission.Start()

	// these have to be done after the injection (of metrics)
	// these are the metrics that libhoney will emit; we preregister them so that they always appear
	libhoneyMetricsName := map[string]string{
		"queue_length":           "gauge",
		"queue_overflow":         "counter",
		"send_errors":            "counter",
		"send_retries":           "counter",
		"batches_sent":           "counter",
		"messages_sent":          "counter",
		"response_decode_errors": "counter",
	}
	for name, typ := range libhoneyMetricsName {
		upstreamMetricsRecorder.Register(name, typ)
		peerMetricsRecorder.Register(name, typ)
	}

	metricsSingleton.Store("UPSTREAM_BUFFER_SIZE", float64(c.GetUpstreamBufferSize()))
	metricsSingleton.Store("PEER_BUFFER_SIZE", float64(c.GetPeerBufferSize()))

	// set up signal channel to exit
	sigsToExit := make(chan os.Signal, 1)
	signal.Notify(sigsToExit, syscall.SIGINT, syscall.SIGTERM)

	// block on our signal handler to exit
	sig := <-sigsToExit
	// unregister ourselves before we go
	close(done)
	time.Sleep(100 * time.Millisecond)
	a.Logger.Error().Logf("Caught signal \"%s\"", sig)
}
