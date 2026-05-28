/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	uberzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/controller"
	fireboltmetrics "github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
	// +kubebuilder:scaffold:imports
)

var version = "dev"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(computev1alpha1.AddToScheme(scheme))

	// +kubebuilder:scaffold:scheme
}

func main() {
	var showVersion bool
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var enableWebhooks bool
	var watchNamespace string
	var engineMaxCPUStr, engineMaxMemoryStr, engineMaxEphemeralStorageStr string
	var tlsOpts []func(*tls.Config)
	flag.BoolVar(&showVersion, "version", false, "Print the version and exit.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", true,
		"Enable the admission webhook server. Disable when TLS certs are not available (e.g. local Kind clusters).")
	flag.StringVar(&watchNamespace, "namespace", "",
		"Namespace to watch for FireboltEngine resources (optional, watches all namespaces if empty)")
	flag.StringVar(&engineMaxCPUStr, "engine-max-cpu", "",
		"Maximum value (Kubernetes resource.Quantity, e.g. \"32\") for FireboltEngine.spec.resources requests/limits CPU. "+
			"Empty disables the bound.")
	flag.StringVar(&engineMaxMemoryStr, "engine-max-memory", "",
		"Maximum value (Kubernetes resource.Quantity, e.g. \"256Gi\") for FireboltEngine.spec.resources requests/limits memory. "+
			"Empty disables the bound.")
	flag.StringVar(&engineMaxEphemeralStorageStr, "engine-max-ephemeral-storage", "",
		"Maximum value (Kubernetes resource.Quantity, e.g. \"10Ti\") for FireboltEngine.spec.resources requests/limits ephemeral-storage. "+
			"Empty disables the bound.")
	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	if showVersion {
		_, _ = fmt.Println(version)
		os.Exit(0)
	}

	ctrl.SetLogger(zap.New(zapLoggerOpts(zapOpts)...))

	engineBounds, boundsErr := parseEngineResourceBounds(engineMaxCPUStr, engineMaxMemoryStr, engineMaxEphemeralStorageStr)
	if boundsErr != nil {
		setupLog.Error(boundsErr, "invalid --engine-max-* flag")
		os.Exit(1)
	}
	if !engineBounds.IsEmpty() {
		setupLog.Info("engine resource bounds enabled",
			"maxCPU", engineBounds.MaxCPU.String(),
			"maxMemory", engineBounds.MaxMemory.String(),
			"maxEphemeralStorage", engineBounds.MaxEphemeralStorage.String())
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	var webhookServer webhook.Server
	if enableWebhooks {
		webhookTLSOpts := tlsOpts
		webhookServerOptions := webhook.Options{
			TLSOpts: webhookTLSOpts,
		}

		if webhookCertPath != "" {
			setupLog.Info("Initializing webhook certificate watcher using provided certificates",
				"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

			webhookServerOptions.CertDir = webhookCertPath
			webhookServerOptions.CertName = webhookCertName
			webhookServerOptions.KeyName = webhookCertKey
		}

		webhookServer = webhook.NewServer(webhookServerOptions)
	}

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if metricsCertPath != "" {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgrOpts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "c662f7f5.firebolt.io",
	}

	if enableWebhooks {
		mgrOpts.WebhookServer = webhookServer
	}

	if watchNamespace != "" {
		mgrOpts.Cache = cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				watchNamespace: {},
			},
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	var engineMetrics fireboltmetrics.EngineRecorder = fireboltmetrics.NoOpEngineRecorder{}
	var instanceMetrics fireboltmetrics.InstanceRecorder = fireboltmetrics.NoOpInstanceRecorder{}
	if metricsAddr != "0" {
		engineMetrics = fireboltmetrics.NewEngineRecorder()
		instanceMetrics = fireboltmetrics.NewInstanceRecorder()
	}

	if err := (&controller.FireboltEngineReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Namespace:       watchNamespace,
		MetricsRecorder: engineMetrics,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "FireboltEngine")
		os.Exit(1)
	}

	if err := (&controller.FireboltInstanceReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		MetricsRecorder: instanceMetrics,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "FireboltInstance")
		os.Exit(1)
	}

	if err := (&controller.EngineClassReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EngineClass")
		os.Exit(1)
	}
	if enableWebhooks {
		if err := computev1alpha1.SetupFireboltInstanceWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "FireboltInstance")
			os.Exit(1)
		}
		if err := computev1alpha1.SetupEngineClassWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "EngineClass")
			os.Exit(1)
		}
		if err := computev1alpha1.SetupFireboltEngineWebhookWithManager(mgr, &engineBounds); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "FireboltEngine")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "version", version)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// zapLoggerOpts builds controller-runtime zap options. Production defaults apply
// when --zap-encoder/--zap-log-level/--zap-stacktrace-level are omitted; pass
// --zap-devel for local console output. Timestamps are always RFC3339.
func zapLoggerOpts(flags zap.Options) []zap.Opts {
	opts := []zap.Opts{
		zap.UseFlagOptions(&flags),
		func(o *zap.Options) {
			o.TimeEncoder = zapcore.RFC3339TimeEncoder
		},
	}
	if flags.Development {
		return opts
	}
	if flags.NewEncoder == nil {
		opts = append(opts, zap.JSONEncoder())
	}
	if flags.Level == nil {
		lvl := uberzap.NewAtomicLevelAt(zapcore.InfoLevel)
		opts = append(opts, zap.Level(&lvl))
	}
	if flags.StacktraceLevel == nil {
		lvl := uberzap.NewAtomicLevelAt(zapcore.ErrorLevel)
		opts = append(opts, zap.StacktraceLevel(&lvl))
	}
	return opts
}

// parseEngineResourceBounds turns the three --engine-max-* flag strings
// into a single EngineResourceBounds value. Each input is parsed as a
// Kubernetes resource.Quantity; an empty string leaves the matching field
// at the zero quantity, which the validator interprets as "no bound".
// A non-empty but malformed value returns an error so the operator fails
// fast at startup instead of silently dropping the bound.
func parseEngineResourceBounds(cpu, memory, ephemeral string) (computev1alpha1.EngineResourceBounds, error) {
	var bounds computev1alpha1.EngineResourceBounds
	if cpu != "" {
		q, err := resource.ParseQuantity(cpu)
		if err != nil {
			return bounds, fmt.Errorf("--engine-max-cpu=%q: %w", cpu, err)
		}
		bounds.MaxCPU = q
	}
	if memory != "" {
		q, err := resource.ParseQuantity(memory)
		if err != nil {
			return bounds, fmt.Errorf("--engine-max-memory=%q: %w", memory, err)
		}
		bounds.MaxMemory = q
	}
	if ephemeral != "" {
		q, err := resource.ParseQuantity(ephemeral)
		if err != nil {
			return bounds, fmt.Errorf("--engine-max-ephemeral-storage=%q: %w", ephemeral, err)
		}
		bounds.MaxEphemeralStorage = q
	}
	return bounds, nil
}
