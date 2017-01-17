/*
 * Copyright (c) 2016 TFG Co <backend@tfgco.com>
 * Author: TFG Co <backend@tfgco.com>
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy of
 * this software and associated documentation files (the "Software"), to deal in
 * the Software without restriction, including without limitation the rights to
 * use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
 * the Software, and to permit persons to whom the Software is furnished to do so,
 * subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
 * FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
 * COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
 * IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
 * CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
 */

package pusher

import (
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/topfreegames/pusher/extensions"
	"github.com/topfreegames/pusher/interfaces"
	"github.com/topfreegames/pusher/util"
)

// APNSPusher struct for apns pusher
type APNSPusher struct {
	AppName                 string
	CertificatePath         string
	Config                  *viper.Viper
	ConfigFile              string
	feedbackReporters       []interfaces.FeedbackReporter
	GracefulShutdownTimeout int
	InvalidTokenHandlers    []interfaces.InvalidTokenHandler
	IsProduction            bool
	Logger                  *logrus.Logger
	MessageHandler          interfaces.MessageHandler
	PendingMessagesWG       *sync.WaitGroup
	Queue                   interfaces.Queue
	run                     bool
	StatsReporters          []interfaces.StatsReporter
}

// NewAPNSPusher for getting a new APNSPusher instance
func NewAPNSPusher(configFile,
	certificatePath,
	appName string,
	isProduction bool,
	logger *logrus.Logger,
	statsReporters []interfaces.StatsReporter,
	db interfaces.DB,
	queueOrNil ...interfaces.APNSPushQueue,
) (*APNSPusher, error) {
	var wg sync.WaitGroup
	a := &APNSPusher{
		AppName:           appName,
		CertificatePath:   certificatePath,
		ConfigFile:        configFile,
		IsProduction:      isProduction,
		Logger:            logger,
		PendingMessagesWG: &wg,
	}
	var queue interfaces.APNSPushQueue
	if len(queueOrNil) > 0 {
		queue = queueOrNil[0]
	}
	err := a.configure(queue, db, statsReporters)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (a *APNSPusher) loadConfigurationDefaults() {
	a.Config.SetDefault("gracefulShutdownTimeout", 10)
}

func (a *APNSPusher) configure(queue interfaces.APNSPushQueue, db interfaces.DB, statsReporters []interfaces.StatsReporter) error {
	a.Config = util.NewViperWithConfigFile(a.ConfigFile)
	a.loadConfigurationDefaults()
	a.GracefulShutdownTimeout = a.Config.GetInt("gracefulShutdownTimeout")
	if err := a.configureStatsReporters(statsReporters); err != nil {
		return err
	}
	if err := a.configureFeedbackReporters(); err != nil {
		return err
	}
	if err := a.configureInvalidTokenHandlers(db); err != nil {
		return err
	}
	q, err := extensions.NewKafkaConsumer(a.Config, a.Logger)
	if err != nil {
		return err
	}
	a.Queue = q
	handler, err := extensions.NewAPNSMessageHandler(
		a.ConfigFile, a.CertificatePath, a.AppName,
		a.IsProduction,
		a.Logger,
		a.Queue.PendingMessagesWaitGroup(),
		a.StatsReporters,
		a.feedbackReporters,
		a.InvalidTokenHandlers,
		queue,
	)
	if err != nil {
		return err
	}
	a.MessageHandler = handler
	return nil
}

func (a *APNSPusher) configureFeedbackReporters() error {
	reporters, err := configureFeedbackReporters(a.ConfigFile, a.Logger, a.Config)
	if err != nil {
		return err
	}
	a.feedbackReporters = reporters
	return nil
}

func (a *APNSPusher) configureStatsReporters(statsReporters []interfaces.StatsReporter) error {
	if statsReporters != nil {
		a.StatsReporters = statsReporters
		return nil
	}
	reporters, err := configureStatsReporters(a.ConfigFile, a.Logger, a.AppName, a.Config)
	if err != nil {
		return err
	}
	a.StatsReporters = reporters
	return nil
}

func (a *APNSPusher) configureInvalidTokenHandlers(dbOrNil interfaces.DB) error {
	invalidTokenHandlers, err := configureInvalidTokenHandlers(a.Config, a.Logger, dbOrNil)
	if err != nil {
		return err
	}
	a.InvalidTokenHandlers = invalidTokenHandlers
	return nil
}

// Start starts pusher in apns mode
func (a *APNSPusher) Start() {
	a.run = true
	l := a.Logger.WithFields(logrus.Fields{
		"method":          "start",
		"configFile":      a.ConfigFile,
		"certificatePath": a.CertificatePath,
	})
	l.Info("starting pusher in apns mode...")
	go a.MessageHandler.HandleMessages(a.Queue.MessagesChannel())
	go a.MessageHandler.HandleResponses()
	go a.Queue.ConsumeLoop()
	go a.reportGoStats()
	sigchan := make(chan os.Signal)
	signal.Notify(sigchan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	for a.run == true {
		select {
		case sig := <-sigchan:
			l.Warnf("caught signal %v: terminating\n", sig)
			a.run = false
		}
	}
	a.Queue.StopConsuming()
	GracefulShutdown(a.Queue.PendingMessagesWaitGroup(), time.Duration(a.GracefulShutdownTimeout)*time.Second)
}

func (a *APNSPusher) reportGoStats() {
	for {
		num := runtime.NumGoroutine()
		m := &runtime.MemStats{}
		runtime.ReadMemStats(m)
		gcTime := m.PauseNs[(m.NumGC+255)%256]
		for _, statsReporter := range a.StatsReporters {
			statsReporter.ReportGoStats(
				num,
				m.Alloc, m.HeapObjects, m.NextGC,
				gcTime,
			)
		}
		time.Sleep(30 * time.Second)
	}
}
