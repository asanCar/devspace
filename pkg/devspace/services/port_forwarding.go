package services

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/loft-sh/devspace/pkg/devspace/config/generated"
	"github.com/loft-sh/devspace/pkg/devspace/deploy/deployer/util"
	"github.com/loft-sh/devspace/pkg/devspace/hook"
	"github.com/loft-sh/devspace/pkg/util/imageselector"

	"github.com/loft-sh/devspace/pkg/devspace/config/versions/latest"
	"github.com/loft-sh/devspace/pkg/devspace/services/targetselector"
	logpkg "github.com/loft-sh/devspace/pkg/util/log"
	"github.com/loft-sh/devspace/pkg/util/message"
	"github.com/loft-sh/devspace/pkg/util/port"
	"github.com/pkg/errors"
)

// StartPortForwarding starts the port forwarding functionality
func (serviceClient *client) StartPortForwarding(interrupt chan error) error {
	if serviceClient.config == nil || serviceClient.config.Config() == nil || serviceClient.config.Generated() == nil {
		return fmt.Errorf("DevSpace config is not set")
	}

	cache := serviceClient.config.Generated().GetActive()
	for _, portForwarding := range serviceClient.config.Config().Dev.Ports {
		if len(portForwarding.PortMappings) == 0 {
			continue
		}

		pluginErr := hook.ExecuteHooks(serviceClient.KubeClient(), serviceClient.Config(), serviceClient.Dependencies(), map[string]interface{}{
			"port_forwarding_config": portForwarding,
		}, serviceClient.log, hook.EventsForSingle("start:portForwarding", portForwarding.Name).With("portForwarding.start")...)
		if pluginErr != nil {
			return pluginErr
		}

		// start port forwarding
		err := serviceClient.startForwarding(cache, portForwarding, interrupt, logpkg.NewUnionLogger(serviceClient.log, logpkg.GetFileLogger("portforwarding")))
		if err != nil {
			pluginErr := hook.ExecuteHooks(serviceClient.KubeClient(), serviceClient.Config(), serviceClient.Dependencies(), map[string]interface{}{
				"port_forwarding_config": portForwarding,
				"error":                  err,
			}, serviceClient.log, hook.EventsForSingle("error:portForwarding", portForwarding.Name).With("portForwarding.error")...)
			if pluginErr != nil {
				return pluginErr
			}

			return err
		}
	}

	return nil
}

func (serviceClient *client) startForwarding(cache *generated.CacheConfig, portForwarding *latest.PortForwardingConfig, interrupt chan error, log logpkg.Logger) error {
	var err error

	// apply config & set image selector
	options := targetselector.NewEmptyOptions().ApplyConfigParameter(portForwarding.LabelSelector, portForwarding.Namespace, "", "")
	options.AllowPick = false
	options.ImageSelector = []imageselector.ImageSelector{}
	if portForwarding.ImageSelector != "" {
		imageSelector, err := util.ResolveImageAsImageSelector(portForwarding.ImageSelector, serviceClient.config, serviceClient.dependencies)
		if err != nil {
			return err
		}

		options.ImageSelector = append(options.ImageSelector, *imageSelector)
	}
	options.WaitingStrategy = targetselector.NewUntilNewestRunningWaitingStrategy(time.Second * 2)
	options.SkipInitContainers = true

	// start port forwarding
	log.StartWait("Port-Forwarding: Waiting for containers to start...")
	pod, err := targetselector.NewTargetSelector(serviceClient.client).SelectSinglePod(context.TODO(), options, log)
	log.StopWait()
	if err != nil {
		return errors.Errorf("%s: %s", message.SelectorErrorPod, err.Error())
	} else if pod == nil {
		return nil
	}

	ports := make([]string, len(portForwarding.PortMappings))
	addresses := make([]string, len(portForwarding.PortMappings))
	for index, value := range portForwarding.PortMappings {
		if value.LocalPort == nil {
			return errors.Errorf("port is not defined in portmapping %d", index)
		}

		localPort := strconv.Itoa(*value.LocalPort)
		remotePort := localPort
		if value.RemotePort != nil {
			remotePort = strconv.Itoa(*value.RemotePort)
		}

		open, _ := port.Check(*value.LocalPort)
		if !open {
			log.Warnf("Seems like port %d is already in use. Is another application using that port?", *value.LocalPort)
		}

		ports[index] = localPort + ":" + remotePort
		if value.BindAddress == "" {
			addresses[index] = "localhost"
		} else {
			addresses[index] = value.BindAddress
		}
	}

	readyChan := make(chan struct{})
	errorChan := make(chan error)

	logFile := logpkg.GetFileLogger("portforwarding")
	pf, err := serviceClient.client.NewPortForwarder(pod, ports, addresses, make(chan struct{}), readyChan, errorChan)
	if err != nil {
		return errors.Errorf("Error starting port forwarding: %v", err)
	}

	go func() {
		err := pf.ForwardPorts()
		if err != nil {
			errorChan <- err
		}
	}()

	// Wait till forwarding is ready
	select {
	case <-readyChan:
		log.Donef("Port forwarding started on %s (%s/%s)", strings.Join(ports, ", "), pod.Namespace, pod.Name)
	case err := <-errorChan:
		return errors.Wrap(err, "forward ports")
	case <-time.After(20 * time.Second):
		return errors.Errorf("Timeout waiting for port forwarding to start")
	}

	go func(portForwarding *latest.PortForwardingConfig, interrupt chan error) {
		select {
		case err := <-errorChan:
			if err != nil {
				logFile.Errorf("Portforwarding restarting, because: %v", err)
				pf.Close()
				hook.LogExecuteHooks(serviceClient.KubeClient(), serviceClient.Config(), serviceClient.Dependencies(), map[string]interface{}{
					"port_forwarding_config": portForwarding,
					"error":                  err,
				}, serviceClient.log, hook.EventsForSingle("restart:portForwarding", portForwarding.Name).With("portForwarding.restart")...)

				for {
					err = serviceClient.startForwarding(cache, portForwarding, interrupt, logFile)
					if err != nil {
						hook.LogExecuteHooks(serviceClient.KubeClient(), serviceClient.Config(), serviceClient.Dependencies(), map[string]interface{}{
							"port_forwarding_config": portForwarding,
							"error":                  err,
						}, serviceClient.log, hook.EventsForSingle("restart:portForwarding", portForwarding.Name).With("portForwarding.restart")...)
						logFile.Errorf("Error restarting port-forwarding: %v", err)
						logFile.Errorf("Will try again in 15 seconds")
						time.Sleep(time.Second * 15)
						continue
					}

					time.Sleep(time.Second * 3)
					break
				}
			}
		case <-interrupt:
			pf.Close()
			hook.LogExecuteHooks(serviceClient.KubeClient(), serviceClient.Config(), serviceClient.Dependencies(), map[string]interface{}{
				"port_forwarding_config": portForwarding,
			}, serviceClient.log, hook.EventsForSingle("stop:portForwarding", portForwarding.Name).With("portForwarding.stop")...)
			logFile.Done("Stopped port forwarding %s", portForwarding.Name)
		}
	}(portForwarding, interrupt)

	return nil
}
