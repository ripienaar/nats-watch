// Copyright (c) 2022, R.I. Pienaar <rip@devco.net>
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	dbg "runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/choria-io/fisk"
	"github.com/nats-io/jsm.go/natscontext"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

var (
	version                                               = "development"
	subjectPrefix                                         = "$NW"
	nameSpace                                             string
	port                                                  int
	listen                                                string
	nctxName                                              string
	sysNctxName                                           string
	logFile                                               string
	debug                                                 bool
	startGossip                                           bool
	startServerPing                                       bool
	name                                                  string
	flakey                                                time.Duration
	log                                                   *logrus.Entry
	ctx                                                   context.Context
	cancel                                                context.CancelFunc
	natsErrorCnt, natsDisconnectionCnt, natsReconnectCnt  prometheus.Counter
	publishErrorCnt, gossipReceivedErrors, statszErrorCnt prometheus.Counter
	statszRequestCnt, errorCnt                            prometheus.Counter
	statszServerCnt                                       *prometheus.CounterVec
	gossipRtt                                             prometheus.Summary
	gossipReceivedDelay, statszServerDelay                *prometheus.SummaryVec
)

func main() {
	app := fisk.New("nats-watch", "NATS Watcher").Action(run)
	app.Version(version)
	app.Author("R.I.Pienaar <rip@devco.net>")

	app.UsageTemplate(fisk.CompactMainUsageTemplate)
	app.ErrorUsageTemplate(fisk.CompactMainUsageTemplate)

	app.Flag("name", "The name of this watcher").Required().StringVar(&name)
	app.Flag("context", "NATS Context for connection").Required().PlaceHolder("NAME").StringVar(&nctxName)
	app.Flag("system-context", "NATS Context for connections requiring a system account").PlaceHolder("NAME").StringVar(&sysNctxName)
	app.Flag("listen", "Host to listen on for Prometheus metrics").Default("localhost").StringVar(&listen)
	app.Flag("port", "Port to listen on for Prometheus metrics").PlaceHolder("PORT").Required().IntVar(&port)
	app.Flag("ns", "Prometheus namespace").Default("nats_watch").PlaceHolder("NS").StringVar(&nameSpace)
	app.Flag("logfile", "File to log to, stdout by default").StringVar(&logFile)
	app.Flag("gossip", "Start the gossip monitor").Default("true").BoolVar(&startGossip)
	app.Flag("server-ping", "Start the nats server ping monitor (requires system account)").UnNegatableBoolVar(&startServerPing)
	app.Flag("jitter", "Introduce random jitter up to duration").PlaceHolder("DURATION").DurationVar(&flakey)
	app.Flag("debug", "Enable debug logging").UnNegatableBoolVar(&debug)

	app.MustParseWithUsage(os.Args[1:])
}

func run(_ *fisk.ParseContext) error {
	if strings.ContainsAny(name, ".>*") {
		return fmt.Errorf("watcher name can not include '.', '>' or '*'")
	}

	rand.Seed(time.Now().UnixNano())

	ctx, cancel = context.WithCancel(context.Background())

	if version == "development" {
		bi, ok := dbg.ReadBuildInfo()
		if ok {
			version = "{{revision}} built {{time}} ({{modified}})"
			for _, kv := range bi.Settings {
				switch kv.Key {
				case "vcs.revision":
					version = strings.Replace(version, "{{revision}}", kv.Value, 1)
				case "vcs.time":
					version = strings.Replace(version, "{{time}}", kv.Value, 1)
				case "vcs.modified":
					if kv.Value == "true" {
						version = strings.Replace(version, "{{modified}}", "dirty", 1)
					} else {
						version = strings.Replace(version, "{{modified}}", "clean", 1)
					}
				}
			}
		}
	}

	err := setupLogging()
	if err != nil {
		return err
	}

	log.Infof("NATS Watcher %s starting on %s:%d", version, listen, port)

	go interruptWatcher()
	go setupPrometheus()

	wg := &sync.WaitGroup{}

	if startGossip {
		wg.Add(1)
		go startGossipPublisher(ctx, wg)

		wg.Add(1)
		go startGossipReceiver(ctx, wg)
	} else {
		log.Warnf("Gossip monitoring disabled")
	}

	if startServerPing {
		wg.Add(1)
		go startStatzMonitor(ctx, wg)
	} else {
		log.Warnf("Server ping monitoring disabled")
	}

	<-ctx.Done()

	wg.Wait()

	return nil
}

func connect(system bool) (*nats.Conn, error) {
	var nctx *natscontext.Context
	var err error

	ctxName := nctxName
	if system && sysNctxName != "" {
		ctxName = sysNctxName
	}

	if strings.HasPrefix(ctxName, string(os.PathSeparator)) {
		nctx, err = natscontext.NewFromFile(ctxName)
	} else {
		nctx, err = natscontext.New(ctxName, true)
	}
	if err != nil {
		return nil, err
	}

	nc, err := nctx.Connect(
		nats.MaxReconnects(-1),
		nats.RetryOnFailedConnect(true),
		nats.NoEcho(),
		nats.CustomInboxPrefix(fmt.Sprintf("%s.reply.%s", subjectPrefix, name)),
		nats.ErrorHandler(func(nc *nats.Conn, sub *nats.Subscription, err error) {
			log.WithFields(logrus.Fields{"nc": nc.ConnectedUrlRedacted(), "sub": sub.Subject}).Errorf("NATS error: %v", err)
			natsErrorCnt.Inc()
			errorCnt.Inc()
		}),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			natsDisconnectionCnt.Inc()
			errorCnt.Inc()
			log.Errorf("NATS disconnected error: %v", err)
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			log.Fatalf("NATS connection closed, terminating watcher")
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			natsReconnectCnt.Inc()
			errorCnt.Inc()
			log.Errorf("NATS reconnected to %v", nc.ConnectedUrlRedacted())
		}),
	)
	if err != nil {
		return nil, err
	}

	return nc, nil
}

func startStatzMonitor(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	nc, err := connect(true)
	if err != nil {
		log.Fatalf("Statsz monitor could not connect to nats: %v", err)
	}

	log.Infof("Starting Statsz monitor")

	ticker := time.NewTicker(5 * time.Second)

	ib := nc.NewInbox()

	sub, err := nc.Subscribe(fmt.Sprintf("%s.*", ib), func(msg *nats.Msg) {
		host := gjson.GetBytes(msg.Data, "server.host")
		if !host.Exists() {
			errorCnt.Inc()
			log.Errorf("server.host not found in update on %s", msg.Subject)
			return
		}
		cluster := gjson.GetBytes(msg.Data, "server.cluster")
		if !cluster.Exists() {
			errorCnt.Inc()
			log.Errorf("server.cluster not found in update on %s", msg.Subject)
			return
		}

		// we extract the last part of the subject as a nanoseconds it was requested by us, delta is the rtt
		parts := strings.Split(msg.Subject, ".")
		nanos, err := strconv.Atoi(parts[len(parts)-1])
		if err != nil {
			errorCnt.Inc()
			log.Errorf("Could not parse timestamp in parts %s: %v", strings.Join(parts, ", "), err)
			return
		}
		sent := time.Unix(0, int64(nanos))
		since := time.Since(sent)
		log.Debugf("Received a statz response from %s with %v delay", host, since)

		statszServerDelay.WithLabelValues(name, host.String(), cluster.String()).Observe(since.Seconds())
		statszServerCnt.WithLabelValues(name, host.String(), cluster.String()).Inc()
	})
	if err != nil {
		log.Fatalf("Subscription failed: %v", err)
		cancel()
		return
	}

	for {
		select {
		case <-ticker.C:
			log.Debugf("Publishing STATSZ request")
			msg := nats.NewMsg("$SYS.REQ.SERVER.PING")
			msg.Reply = fmt.Sprintf("%s.%d", ib, time.Now().UnixNano())

			statszRequestCnt.Inc()
			err = nc.PublishMsg(msg)
			if err != nil {
				log.Errorf("STATSZ publishing failed: %v", err)
				statszErrorCnt.Inc()
				publishErrorCnt.Inc()
				errorCnt.Inc()
			}

		case <-ctx.Done():
			log.Infof("STATSZ monitor exiting on context interrupr")
			sub.Unsubscribe()
			return
		}
	}
}

func startGossipReceiver(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	nc, err := connect(false)
	if err != nil {
		log.Fatalf("Gossip receiver could not connect to nats: %v", err)
	}

	subj := fmt.Sprintf("%s.gossip.>", subjectPrefix)
	log.Infof("Starting Gossip receiver on subject %s", subj)

	sub, err := nc.Subscribe(subj, func(msg *nats.Msg) {
		ts, err := time.Parse(time.RFC3339Nano, string(msg.Data))
		if err != nil {
			gossipReceivedErrors.Inc()
			errorCnt.Inc()
			return
		}

		tokens := strings.Split(msg.Subject, ".")
		if len(tokens) == 0 {
			gossipReceivedErrors.Inc()
			errorCnt.Inc()
			return
		}

		gossipReceivedDelay.WithLabelValues(name, tokens[len(tokens)-1]).Observe(time.Since(ts).Seconds())
	})
	if err != nil {
		log.Fatalf("Subscription failed: %v", err)
		cancel()
		return
	}

	<-ctx.Done()

	log.Infof("Gossip receiver exiting on context interrupr")
	sub.Unsubscribe()
}

func startGossipPublisher(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	nc, err := connect(false)
	if err != nil {
		log.Fatalf("Gossip publisher could not connect to nats: %v", err)
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	subj := fmt.Sprintf("%s.gossip.%s", subjectPrefix, name)

	log.Infof("Starting Gossip publisher on subject %s", subj)

	gossip := func() {
		obs := prometheus.NewTimer(gossipRtt)
		defer obs.ObserveDuration()

		log.Debugf("Publishing gossip")

		now := time.Now().UTC()
		if flakey > 0 {
			delay := -1 * time.Duration(rand.Intn(int(flakey.Milliseconds()))) * time.Millisecond
			log.Debugf("Skewing time in message by %v", delay)
			now = now.Add(delay)
		}

		err := nc.Publish(subj, []byte(now.Format(time.RFC3339Nano)))
		if err != nil {
			log.Errorf("Gossip publishing failed: %v", err)
			publishErrorCnt.Inc()
			errorCnt.Inc()
		}

		timeout, cancel := context.WithTimeout(ctx, time.Second)
		err = nc.FlushWithContext(timeout)
		cancel()
		if err != nil {
			log.Errorf("Gossip flush failed: %v", err)
			publishErrorCnt.Inc()
			errorCnt.Inc()
		}
	}

	for {
		select {
		case <-ticker.C:
			gossip()

		case <-ctx.Done():
			log.Infof("Gossip publisher exiting on context interrupr")
			return
		}
	}
}

func setupLogging() error {
	logger := logrus.New()
	if logFile != "" {
		of, err := os.Open(logFile)
		if err != nil {
			return err
		}

		logger.SetOutput(of)
	}

	if debug {
		logger.SetLevel(logrus.DebugLevel)
	}

	log = logrus.NewEntry(logger).WithFields(logrus.Fields{"name": name, "context": nctxName, "port": port})

	return nil
}

func setupPrometheus() {
	errorCnt = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        prometheus.BuildFQName(nameSpace, "watcher", "error_count"),
		Help:        "Total number of errors encountered",
		ConstLabels: prometheus.Labels{"name": name},
	})
	prometheus.MustRegister(errorCnt)

	natsErrorCnt = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        prometheus.BuildFQName(nameSpace, "nats", "error_count"),
		Help:        "Number of times the nats error handler got called",
		ConstLabels: prometheus.Labels{"name": name},
	})
	prometheus.MustRegister(natsErrorCnt)

	natsDisconnectionCnt = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        prometheus.BuildFQName(nameSpace, "nats", "disconnection_count"),
		Help:        "Number of times the nats disconnection handler got called",
		ConstLabels: prometheus.Labels{"name": name},
	})
	prometheus.MustRegister(natsDisconnectionCnt)

	natsReconnectCnt = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        prometheus.BuildFQName(nameSpace, "nats", "reconnection_count"),
		Help:        "Number of times the nats reconnect handler got called",
		ConstLabels: prometheus.Labels{"name": name},
	})
	prometheus.MustRegister(natsReconnectCnt)

	publishErrorCnt = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        prometheus.BuildFQName(nameSpace, "nats", "publish_error_count"),
		Help:        "Number messages that couldn't not be published",
		ConstLabels: prometheus.Labels{"name": name},
	})
	prometheus.MustRegister(publishErrorCnt)

	gossipRtt = prometheus.NewSummary(prometheus.SummaryOpts{
		Name:        prometheus.BuildFQName(nameSpace, "gossip", "rtt"),
		Help:        "he delay between a gossip message being published and it being received",
		ConstLabels: prometheus.Labels{"name": name},
	})
	prometheus.MustRegister(gossipRtt)

	gossipReceivedDelay = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name: prometheus.BuildFQName(nameSpace, "gossip", "received_delay"),
		Help: "Delay in receiving a message from another watcher",
	}, []string{"name", "sender"})
	prometheus.MustRegister(gossipReceivedDelay)

	gossipReceivedErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        prometheus.BuildFQName(nameSpace, "gossip", "received_errors"),
		Help:        "Number of invalid messages received",
		ConstLabels: prometheus.Labels{"name": name},
	})
	prometheus.MustRegister(gossipReceivedErrors)

	statszServerDelay = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name: prometheus.BuildFQName(nameSpace, "statsz", "server_response_delay"),
		Help: "Delay in receiving a message from a server",
	}, []string{"name", "server", "cluster"})
	prometheus.MustRegister(statszServerDelay)

	statszServerCnt = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prometheus.BuildFQName(nameSpace, "statsz", "server_response_count"),
		Help: "Number responses received from a server",
	}, []string{"name", "server", "cluster"})
	prometheus.MustRegister(statszServerCnt)

	statszRequestCnt = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        prometheus.BuildFQName(nameSpace, "statsz", "requests_count"),
		Help:        "Number of stats requests that were published",
		ConstLabels: prometheus.Labels{"name": name},
	})
	prometheus.MustRegister(statszRequestCnt)

	buildInfo := prometheus.NewCounter(prometheus.CounterOpts{
		Name:        prometheus.BuildFQName(nameSpace, "build", "info"),
		Help:        "Build information about the running server",
		ConstLabels: prometheus.Labels{"name": name, "version": version},
	})
	prometheus.MustRegister(buildInfo)
	buildInfo.Inc()

	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(fmt.Sprintf("%s:%d", listen, port), nil))
}

func forceQuit() {
	<-time.After(10 * time.Second)

	log.Errorf("Forced shutdown triggered")

	os.Exit(1)
}

func interruptWatcher() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case sig := <-sigs:
			go forceQuit()

			log.Infof("Shutting down on %s", sig)
			cancel()

		case <-ctx.Done():
			return
		}
	}
}
