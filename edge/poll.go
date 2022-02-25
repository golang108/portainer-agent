package edge

import (
	"encoding/base64"
	"github.com/portainer/agent/edge/client"
	"log"
	"strconv"
	"time"

	"github.com/portainer/agent"
	"github.com/portainer/agent/chisel"
	"github.com/portainer/agent/edge/scheduler"
	"github.com/portainer/agent/edge/stack"
	"github.com/portainer/libcrypto"
)

const tunnelActivityCheckInterval = 30 * time.Second

// PollService is used to poll a Portainer instance to retrieve the status associated to the Edge endpoint.
// It is responsible for managing the state of the reverse tunnel (open and closing after inactivity).
// It is also responsible for retrieving the data associated to Edge stacks and schedules.
type PollService struct {
	apiServerAddr           string
	pollIntervalInSeconds   float64
	inactivityTimeout       time.Duration
	edgeID                  string
	portainerClient         client.PortainerClient
	tunnelClient            agent.ReverseTunnelClient
	scheduleManager         agent.Scheduler
	lastActivity            time.Time
	refreshSignal           chan struct{}
	edgeStackManager        *stack.StackManager
	portainerURL            string
	endpointID              string
	tunnelServerAddr        string
	tunnelServerFingerprint string
	logsManager             *scheduler.LogsManager
}

type pollServiceConfig struct {
	APIServerAddr           string
	EdgeID                  string
	InactivityTimeout       string
	PollFrequency           string
	TunnelCapability        bool
	PortainerURL            string
	EndpointID              string
	TunnelServerAddr        string
	TunnelServerFingerprint string
	EdgeAsyncMode           bool
}

// newPollService returns a pointer to a new instance of PollService
// if TunnelCapability is disabled, it will only poll for Edge stacks and schedule without managing reverse tunnels.
func newPollService(edgeStackManager *stack.StackManager, logsManager *scheduler.LogsManager, config *pollServiceConfig, portainerClient client.PortainerClient) (*PollService, error) {
	pollFrequency, err := time.ParseDuration(config.PollFrequency)
	if err != nil {
		return nil, err
	}

	inactivityTimeout, err := time.ParseDuration(config.InactivityTimeout)
	if err != nil {
		return nil, err
	}

	var tunnel agent.ReverseTunnelClient
	if config.TunnelCapability && !config.EdgeAsyncMode {
		tunnel = chisel.NewClient()
	}

	pollService := &PollService{
		apiServerAddr:           config.APIServerAddr,
		edgeID:                  config.EdgeID,
		pollIntervalInSeconds:   pollFrequency.Seconds(),
		inactivityTimeout:       inactivityTimeout,
		tunnelClient:            tunnel,
		scheduleManager:         scheduler.NewCronManager(),
		refreshSignal:           nil,
		edgeStackManager:        edgeStackManager,
		portainerURL:            config.PortainerURL,
		endpointID:              config.EndpointID,
		tunnelServerAddr:        config.TunnelServerAddr,
		tunnelServerFingerprint: config.TunnelServerFingerprint,
		logsManager:             logsManager,
		portainerClient:         portainerClient,
	}

	return pollService, nil
}

func (service *PollService) resetActivityTimer() {
	if service.tunnelClient != nil && service.tunnelClient.IsTunnelOpen() {
		service.lastActivity = time.Now()
	}
}

// start will start two loops in go routines
// The first loop will poll the Portainer instance for the status of the associated endpoint and create a reverse tunnel
// if needed as well as manage schedules.
// The second loop will check for the last activity of the reverse tunnel and close the tunnel if it exceeds the tunnel
// inactivity duration.
func (service *PollService) start() error {
	if service.refreshSignal != nil {
		return nil
	}

	service.refreshSignal = make(chan struct{})
	service.startStatusPollLoop()
	service.startActivityMonitoringLoop()

	return nil
}

func (service *PollService) stop() error {
	if service.refreshSignal != nil {
		close(service.refreshSignal)
		service.refreshSignal = nil
	}
	return nil
}

func (service *PollService) restartStatusPollLoop() {
	service.stop()
	service.refreshSignal = make(chan struct{})
	service.startStatusPollLoop()
}

func (service *PollService) startStatusPollLoop() error {
	log.Printf("[DEBUG] [edge] [poll_interval_seconds: %f] [server_url: %s] [message: starting Portainer short-polling client]", service.pollIntervalInSeconds, service.portainerURL)

	ticker := time.NewTicker(time.Duration(service.pollIntervalInSeconds) * time.Second)
	go func() {
		for {
			select {
			case <-ticker.C:
				err := service.poll()
				if err != nil {
					log.Printf("[ERROR] [edge] [message: an error occured during short poll] [error: %s]", err)
				}

			case <-service.refreshSignal:
				log.Println("[DEBUG] [edge] [message: shutting down Portainer short-polling client]")
				ticker.Stop()
				return
			}
		}
	}()

	return nil
}

func (service *PollService) startActivityMonitoringLoop() {
	if service.tunnelClient == nil {
		return
	}

	ticker := time.NewTicker(tunnelActivityCheckInterval)
	quit := make(chan struct{})

	log.Printf("[DEBUG] [edge] [monitoring_interval_seconds: %f] [inactivity_timeout: %s] [message: starting activity monitoring loop]", tunnelActivityCheckInterval.Seconds(), service.inactivityTimeout.String())

	go func() {
		for {
			select {
			case <-ticker.C:

				if service.lastActivity.IsZero() {
					continue
				}

				elapsed := time.Since(service.lastActivity)
				log.Printf("[DEBUG] [edge] [tunnel_last_activity_seconds: %f] [message: tunnel activity monitoring]", elapsed.Seconds())

				if service.tunnelClient != nil && service.tunnelClient.IsTunnelOpen() && elapsed.Seconds() > service.inactivityTimeout.Seconds() {

					log.Printf("[INFO] [edge] [tunnel_last_activity_seconds: %f] [message: shutting down tunnel after inactivity period]", elapsed.Seconds())

					err := service.tunnelClient.CloseTunnel()
					if err != nil {
						log.Printf("[ERROR] [edge] [message: unable to shutdown tunnel] [error: %s]", err)
					}
				}

			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()
}

const clientDefaultPollTimeout = 5

func (service *PollService) poll() error {
	responseData, err := service.portainerClient.GetEnvironmentStatus()
	if err != nil {
		return err
	}

	log.Printf("[DEBUG] [edge] [status: %s] [port: %d] [schedule_count: %d] [checkin_interval_seconds: %f]", responseData.Status, responseData.Port, len(responseData.Schedules), responseData.CheckinInterval)

	if service.tunnelClient != nil {
		if responseData.Status == "IDLE" && service.tunnelClient.IsTunnelOpen() {
			log.Printf("[DEBUG] [edge] [status: %s] [message: Idle status detected, shutting down tunnel]", responseData.Status)

			err := service.tunnelClient.CloseTunnel()
			if err != nil {
				log.Printf("[ERROR] [edge] [message: Unable to shutdown tunnel] [error: %s]", err)
			}
		}

		if responseData.Status == "REQUIRED" && !service.tunnelClient.IsTunnelOpen() {
			log.Println("[DEBUG] [edge] [message: Required status detected, creating reverse tunnel]")

			err := service.createTunnel(responseData.Credentials, responseData.Port)
			if err != nil {
				log.Printf("[ERROR] [edge] [message: Unable to create tunnel] [error: %s]", err)
				return err
			}
		}
	}

	err = service.scheduleManager.Schedule(responseData.Schedules)
	if err != nil {
		log.Printf("[ERROR] [edge] [message: an error occurred during schedule management] [err: %s]", err)
	}

	logsToCollect := []int{}
	for _, schedule := range responseData.Schedules {
		if schedule.CollectLogs {
			logsToCollect = append(logsToCollect, schedule.ID)
		}
	}

	service.logsManager.HandleReceivedLogsRequests(logsToCollect)

	if responseData.CheckinInterval > 0 && responseData.CheckinInterval != service.pollIntervalInSeconds {
		log.Printf("[DEBUG] [edge] [old_interval: %f] [new_interval: %f] [message: updating poll interval]", service.pollIntervalInSeconds, responseData.CheckinInterval)
		service.pollIntervalInSeconds = responseData.CheckinInterval
		service.portainerClient.SetTimeout(time.Duration(responseData.CheckinInterval) * time.Second)
		go service.restartStatusPollLoop()
	}

	if responseData.Stacks != nil {
		stacks := map[int]int{}
		for _, stack := range responseData.Stacks {
			stacks[stack.ID] = stack.Version
		}

		err := service.edgeStackManager.UpdateStacksStatus(stacks)
		if err != nil {
			log.Printf("[ERROR] [edge] [message: an error occurred during stack management] [error: %s]", err)
			return err
		}
	}

	return nil
}

func (service *PollService) createTunnel(encodedCredentials string, remotePort int) error {
	if service.tunnelClient == nil {
		return nil
	}

	decodedCredentials, err := base64.RawStdEncoding.DecodeString(encodedCredentials)
	if err != nil {
		return err
	}

	credentials, err := libcrypto.Decrypt(decodedCredentials, []byte(service.edgeID))
	if err != nil {
		return err
	}

	tunnelConfig := agent.TunnelConfig{
		ServerAddr:       service.tunnelServerAddr,
		ServerFingerpint: service.tunnelServerFingerprint,
		Credentials:      string(credentials),
		RemotePort:       strconv.Itoa(remotePort),
		LocalAddr:        service.apiServerAddr,
	}

	err = service.tunnelClient.CreateTunnel(tunnelConfig)
	if err != nil {
		return err
	}

	service.resetActivityTimer()
	return nil
}
