// Copyright (c) 2025 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"os"
	"time"

	libboltkv "github.com/bborbe/boltkv"
	"github.com/bborbe/errors"
	libhttp "github.com/bborbe/http"
	libkafka "github.com/bborbe/kafka"
	libkv "github.com/bborbe/kv"
	"github.com/bborbe/log"
	libmetrics "github.com/bborbe/metrics"
	"github.com/bborbe/run"
	libsentry "github.com/bborbe/sentry"
	"github.com/bborbe/service"
	libtime "github.com/bborbe/time"
	"github.com/golang/glog"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/bborbe/a2a-worker-controller/pkg/factory"
)

const serviceName = "a2a-worker-controller"

func main() {
	app := &application{}
	os.Exit(service.Main(context.Background(), app, &app.SentryDSN, &app.SentryProxy))
}

type application struct {
	SentryDSN       string            `required:"true"  arg:"sentry-dsn"        env:"SENTRY_DSN"        usage:"SentryDSN"                             display:"length"`
	SentryProxy     string            `required:"false" arg:"sentry-proxy"      env:"SENTRY_PROXY"      usage:"Sentry Proxy"`
	Listen          string            `required:"true"  arg:"listen"            env:"LISTEN"            usage:"address to listen to"`
	KafkaBrokers    string            `required:"true"  arg:"kafka-brokers"     env:"KAFKA_BROKERS"     usage:"Comma separated list of Kafka brokers"`
	BatchSize       int               `required:"true"  arg:"batch-size"        env:"BATCH_SIZE"        usage:"batch consume size"                                     default:"1"`
	DataDir         string            `required:"true"  arg:"datadir"           env:"DATADIR"           usage:"data directory"`
	BuildGitVersion string            `required:"false" arg:"build-git-version" env:"BUILD_GIT_VERSION" usage:"Build Git version"                                      default:"dev"`
	BuildGitCommit  string            `required:"false" arg:"build-git-commit"  env:"BUILD_GIT_COMMIT"  usage:"Build Git commit hash"                                  default:"none"`
	BuildDate       *libtime.DateTime `required:"false" arg:"build-date"        env:"BUILD_DATE"        usage:"Build timestamp (RFC3339)"`
}

func (a *application) Run(ctx context.Context, sentryClient libsentry.Client) error {
	libmetrics.NewBuildInfoMetrics().SetBuildInfo(a.BuildGitVersion, a.BuildGitCommit, a.BuildDate)

	saramaClient, err := libkafka.CreateSaramaClient(
		ctx,
		libkafka.ParseBrokersFromString(a.KafkaBrokers),
	)
	if err != nil {
		return errors.Wrap(ctx, err, "create sarama client failed")
	}
	defer saramaClient.Close()

	syncProducer, err := libkafka.NewSyncProducerWithName(
		ctx,
		libkafka.ParseBrokersFromString(a.KafkaBrokers),
		serviceName,
	)
	if err != nil {
		return errors.Wrap(ctx, err, "create sync producer failed")
	}
	defer syncProducer.Close()

	db, err := libboltkv.OpenDir(ctx, a.DataDir)
	if err != nil {
		return errors.Wrap(ctx, err, "open db failed")
	}
	defer db.Close()

	return service.Run(
		ctx,
		a.createHTTPServer(sentryClient, db),
	)

}

func (a *application) createHTTPServer(
	sentryClient libsentry.Client,
	db libkv.DB,
) run.Func {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		router := mux.NewRouter()
		router.Path("/healthz").Handler(factory.CreateHealthzHandler())
		router.Path("/readiness").Handler(libhttp.NewPrintHandler("OK"))
		router.Path("/metrics").Handler(promhttp.Handler())
		router.Path("/resetdb").Handler(libkv.NewResetHandler(db, cancel))
		router.Path("/resetbucket/{BucketName}").Handler(libkv.NewResetBucketHandler(db, cancel))
		router.Path("/setloglevel/{level}").
			Handler(log.NewSetLoglevelHandler(ctx, log.NewLogLevelSetter(2, 5*time.Minute)))
		router.Path("/gc").Handler(libhttp.NewGarbageCollectorHandler())
		router.Path("/testloglevel").Handler(factory.CreateTestLoglevelHandler())
		router.Path("/sentryalert").Handler(factory.CreateSentryAlertHandler(sentryClient))

		glog.V(2).Infof("starting http server listen on %s", a.Listen)
		return libhttp.NewServer(
			a.Listen,
			router,
		).Run(ctx)
	}
}
