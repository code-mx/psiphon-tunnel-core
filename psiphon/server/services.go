/*
 * Copyright (c) 2016, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

// Package psiphon/server implements the core tunnel functionality of a Psiphon server.
// The main function is RunServices, which runs one or all of a Psiphon API web server,
// a tunneling SSH server, and an Obfuscated SSH protocol server. The server configuration
// is created by the GenerateConfig function.
package server

import (
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/server/psinet"
)

// RunServices initializes support functions including logging and GeoIP services;
// and then starts the server components and runs them until os.Interrupt or
// os.Kill signals are received. The config determines which components are run.
func RunServices(configJSON []byte) error {

	config, err := LoadConfig(configJSON)
	if err != nil {
		log.WithContextFields(LogFields{"error": err}).Error("load config failed")
		return psiphon.ContextError(err)
	}

	err = InitLogging(config)
	if err != nil {
		log.WithContextFields(LogFields{"error": err}).Error("init logging failed")
		return psiphon.ContextError(err)
	}

	supportServices, err := NewSupportServices(config)
	if err != nil {
		log.WithContextFields(LogFields{"error": err}).Error("init support services failed")
		return psiphon.ContextError(err)
	}

	waitGroup := new(sync.WaitGroup)
	shutdownBroadcast := make(chan struct{})
	errors := make(chan error)

	tunnelServer, err := NewTunnelServer(supportServices, shutdownBroadcast)
	if err != nil {
		log.WithContextFields(LogFields{"error": err}).Error("init tunnel server failed")
		return psiphon.ContextError(err)
	}

	if config.RunLoadMonitor() {
		waitGroup.Add(1)
		go func() {
			waitGroup.Done()
			ticker := time.NewTicker(time.Duration(config.LoadMonitorPeriodSeconds) * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-shutdownBroadcast:
					return
				case <-ticker.C:
					logServerLoad(tunnelServer)
				}
			}
		}()
	}

	if config.RunWebServer() {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			err := RunWebServer(supportServices, shutdownBroadcast)
			select {
			case errors <- err:
			default:
			}
		}()
	}

	// The tunnel server is always run; it launches multiple
	// listeners, depending on which tunnel protocols are enabled.
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		err := tunnelServer.Run()
		select {
		case errors <- err:
		default:
		}
	}()

	// An OS signal triggers an orderly shutdown
	systemStopSignal := make(chan os.Signal, 1)
	signal.Notify(systemStopSignal, os.Interrupt, os.Kill)

	// SIGUSR1 triggers a reload of support services
	reloadSupportServicesSignal := make(chan os.Signal, 1)
	signal.Notify(reloadSupportServicesSignal, syscall.SIGUSR1)

	// SIGUSR2 triggers an immediate load log
	logServerLoadSignal := make(chan os.Signal, 1)
	signal.Notify(logServerLoadSignal, syscall.SIGUSR2)

	err = nil

loop:
	for {
		select {
		case <-reloadSupportServicesSignal:
			supportServices.Reload()
		case <-logServerLoadSignal:
			logServerLoad(tunnelServer)
		case <-systemStopSignal:
			log.WithContext().Info("shutdown by system")
			break loop
		case err = <-errors:
			log.WithContextFields(LogFields{"error": err}).Error("service failed")
			break loop
		}
	}

	close(shutdownBroadcast)
	waitGroup.Wait()

	return err
}

func logServerLoad(server *TunnelServer) {

	// golang runtime stats
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	fields := LogFields{
		"NumGoroutine":        runtime.NumGoroutine(),
		"MemStats.Alloc":      memStats.Alloc,
		"MemStats.TotalAlloc": memStats.TotalAlloc,
		"MemStats.Sys":        memStats.Sys,
	}

	// tunnel server stats
	for tunnelProtocol, stats := range server.GetLoadStats() {
		for stat, value := range stats {
			fields[tunnelProtocol+"."+stat] = value
		}
	}

	log.WithContextFields(fields).Info("load")
}

// SupportServices carries common and shared data components
// across different server components. SupportServices implements a
// hot reload of traffic rules, psinet database, and geo IP database
// components, which allows these data components to be refreshed
// without restarting the server process.
type SupportServices struct {
	Config          *Config
	TrafficRulesSet *TrafficRulesSet
	PsinetDatabase  *psinet.Database
	GeoIPService    *GeoIPService
}

// NewSupportServices initializes a new SupportServices.
func NewSupportServices(config *Config) (*SupportServices, error) {
	trafficRulesSet, err := NewTrafficRulesSet(config.TrafficRulesFilename)
	if err != nil {
		return nil, psiphon.ContextError(err)
	}

	psinetDatabase, err := psinet.NewDatabase(config.PsinetDatabaseFilename)
	if err != nil {
		return nil, psiphon.ContextError(err)
	}

	geoIPService, err := NewGeoIPService(
		config.GeoIPDatabaseFilename, config.DiscoveryValueHMACKey)
	if err != nil {
		return nil, psiphon.ContextError(err)
	}

	return &SupportServices{
		Config:          config,
		TrafficRulesSet: trafficRulesSet,
		PsinetDatabase:  psinetDatabase,
		GeoIPService:    geoIPService,
	}, nil
}

// Reload reinitializes traffic rules, psinet database, and geo IP database
// components. If any component fails to reload, an error is logged and
// Reload proceeds, using the previous state of the component.
//
// Note: reload of traffic rules currently doesn't apply to existing,
// established clients.
//
func (support *SupportServices) Reload() {

	if support.Config.TrafficRulesFilename != "" {
		err := support.TrafficRulesSet.Reload(support.Config.TrafficRulesFilename)
		if err != nil {
			log.WithContextFields(LogFields{"error": err}).Error("reload traffic rules failed")
			// Keep running with previous state of support.TrafficRulesSet
		} else {
			log.WithContext().Info("reloaded traffic rules")
		}
	}

	if support.Config.PsinetDatabaseFilename != "" {
		err := support.PsinetDatabase.Reload(support.Config.PsinetDatabaseFilename)
		if err != nil {
			log.WithContextFields(LogFields{"error": err}).Error("reload psinet database failed")
			// Keep running with previous state of support.PsinetDatabase
		} else {
			log.WithContext().Info("reloaded psinet database")
		}
	}

	if support.Config.GeoIPDatabaseFilename != "" {
		err := support.GeoIPService.ReloadDatabase(support.Config.GeoIPDatabaseFilename)
		if err != nil {
			log.WithContextFields(LogFields{"error": err}).Error("reload GeoIP database failed")
			// Keep running with previous state of support.GeoIPService
		} else {
			log.WithContext().Info("reloaded GeoIP database")
		}
	}
}
