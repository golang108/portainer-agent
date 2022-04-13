package http

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/portainer/agent"
	"github.com/portainer/agent/edge"
	"github.com/portainer/agent/exec"
	"github.com/portainer/agent/http/handler"
	"github.com/portainer/agent/kubernetes"
	httperror "github.com/portainer/libhttp/error"
)

// APIServer is the web server exposing the API of an agent.
type APIServer struct {
	addr               string
	port               string
	systemService      agent.SystemService
	clusterService     agent.ClusterService
	signatureService   agent.DigitalSignatureService
	edgeManager        *edge.Manager
	agentTags          *agent.RuntimeConfiguration
	agentOptions       *agent.Options
	kubeClient         *kubernetes.KubeClient
	kubernetesDeployer *exec.KubernetesDeployer
	containerPlatform  agent.ContainerPlatform
}

// APIServerConfig represents a server configuration
// used to create a new API server
type APIServerConfig struct {
	Addr                 string
	Port                 string
	SystemService        agent.SystemService
	ClusterService       agent.ClusterService
	SignatureService     agent.DigitalSignatureService
	EdgeManager          *edge.Manager
	KubeClient           *kubernetes.KubeClient
	KubernetesDeployer   *exec.KubernetesDeployer
	RuntimeConfiguration *agent.RuntimeConfiguration
	AgentOptions         *agent.Options
	ContainerPlatform    agent.ContainerPlatform
}

// NewAPIServer returns a pointer to a APIServer.
func NewAPIServer(config *APIServerConfig) *APIServer {
	return &APIServer{
		addr:               config.Addr,
		port:               config.Port,
		systemService:      config.SystemService,
		clusterService:     config.ClusterService,
		signatureService:   config.SignatureService,
		edgeManager:        config.EdgeManager,
		agentTags:          config.RuntimeConfiguration,
		agentOptions:       config.AgentOptions,
		kubeClient:         config.KubeClient,
		kubernetesDeployer: config.KubernetesDeployer,
		containerPlatform:  config.ContainerPlatform,
	}
}

func (server *APIServer) enhanceAPIForEdgeMode(next http.Handler, isSecure bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isSecure && !server.edgeManager.IsKeySet() {
			httperror.WriteError(w, http.StatusForbidden, "Unable to use the unsecured agent API without Edge key", errors.New("edge key not set"))
			return
		}

		server.edgeManager.ResetActivityTimer()

		next.ServeHTTP(w, r)
	})
}

// Start starts a new web server by listening on the specified listenAddr.
func (server *APIServer) StartUnsecured(edgeMode bool) error {
	config := &handler.Config{
		SystemService:        server.systemService,
		ClusterService:       server.clusterService,
		SignatureService:     server.signatureService,
		RuntimeConfiguration: server.agentTags,
		EdgeManager:          server.edgeManager,
		KubeClient:           server.kubeClient,
		KubernetesDeployer:   server.kubernetesDeployer,
		Secured:              false,
		ContainerPlatform:    server.containerPlatform,
	}

	var h http.Handler = handler.NewHandler(config)
	listenAddr := server.addr + ":" + server.port

	if edgeMode {
		h = server.enhanceAPIForEdgeMode(h, false)
	}

	log.Printf("[INFO] [http] [server_addr: %s] [server_port: %s] [secured: %t] [api_version: %s] [message: Starting Agent API server]", server.addr, server.port, config.Secured, agent.Version)

	httpServer := &http.Server{
		Addr:         listenAddr,
		Handler:      h,
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 30 * time.Minute,
	}

	return httpServer.ListenAndServe()
}

// Start starts a new web server by listening on the specified listenAddr.
func (server *APIServer) StartSecured(edgeMode bool) error {
	config := &handler.Config{
		SystemService:        server.systemService,
		ClusterService:       server.clusterService,
		SignatureService:     server.signatureService,
		RuntimeConfiguration: server.agentTags,
		EdgeManager:          server.edgeManager,
		KubeClient:           server.kubeClient,
		KubernetesDeployer:   server.kubernetesDeployer,
		Secured:              true,
		ContainerPlatform:    server.containerPlatform,
	}

	var h http.Handler = handler.NewHandler(config)
	listenAddr := server.addr + ":" + server.port

	if edgeMode {
		h = server.enhanceAPIForEdgeMode(h, true)
	}

	log.Printf("[INFO] [http] [server_addr: %s] [server_port: %s] [secured: %t] [api_version: %s] [message: Starting Agent API server]", server.addr, server.port, config.Secured, agent.Version)

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
	}

	httpServer := &http.Server{
		Addr:         listenAddr,
		Handler:      h,
		ReadTimeout:  120 * time.Second,
		TLSConfig:    tlsConfig,
		WriteTimeout: 30 * time.Minute,
	}

	go func() {
		securityShutdown := server.agentOptions.AgentSecurityShutdown
		time.Sleep(securityShutdown)

		if !server.signatureService.IsAssociated() {
			log.Printf("[INFO] [main,http] [message: Shutting down API server as no client was associated after %s, keeping alive to prevent restart by docker/kubernetes]", securityShutdown)

			err := httpServer.Shutdown(context.Background())
			if err != nil {
				log.Fatalf("[ERROR] [server] [message: failed shutting down server] [error: %s]", err)
			}

		}
	}()

	return httpServer.ListenAndServeTLS(agent.TLSCertPath, agent.TLSKeyPath)
}
