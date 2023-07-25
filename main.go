/*
Copyright 2021 SPIRE Authors.

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
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/spiffe/go-spiffe/v2/spiffeid"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/spiffe/spire-controller-manager/controllers"
	"github.com/spiffe/spire-controller-manager/pkg/spireapi"
	"github.com/spiffe/spire-controller-manager/pkg/spireentry"
	"github.com/spiffe/spire-controller-manager/pkg/spirefederationrelationship"
	"github.com/spiffe/spire-controller-manager/pkg/webhookmanager"
	//+kubebuilder:scaffold:imports
)

const (
	defaultSPIREServerSocketPath = "/spire-server/api.sock"
	defaultGCInterval            = 10 * time.Second
	k8sDefaultService            = "kubernetes.default.svc"
)

var (
	scheme                 = runtime.NewScheme()
	setupLog               = ctrl.Log.WithName("setup")
	customResourcesPresent RequiredCustomResources
)

type RequiredCustomResources struct {
	ClusterStaticEntryPresent          bool
	ClusterSpiffeIDPresent             bool
	ClusterFederatedTrustDomainPresent bool
}

func (r *RequiredCustomResources) fullyInitialized() bool {
	return r.ClusterSpiffeIDPresent && r.ClusterStaticEntryPresent && r.ClusterFederatedTrustDomainPresent
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(spirev1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	ctrlConfig, options, ignoreNamespacesRegex, err := parseConfig()
	if err != nil {
		setupLog.Error(err, "error parsing configuration")
		os.Exit(1)
	}

	if err := run(ctrlConfig, options, ignoreNamespacesRegex); err != nil {
		os.Exit(1)
	}
}

func parseConfig() (spirev1alpha1.ControllerManagerConfig, ctrl.Options, []*regexp.Regexp, error) {
	var configFileFlag string
	var spireAPISocketFlag string
	flag.StringVar(&configFileFlag, "config", "",
		"The controller will load its initial configuration from this file. "+
			"Omit this flag to use the default configuration values. "+
			"Command-line flags override configuration from this file.")
	flag.StringVar(&spireAPISocketFlag, "spire-api-socket", "", "The path to the SPIRE API socket (deprecated; use the config file)")

	// Parse log flags
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Set default values
	ctrlConfig := spirev1alpha1.ControllerManagerConfig{
		IgnoreNamespaces:                   []string{"kube-system", "kube-public", "spire-system"},
		GCInterval:                         defaultGCInterval,
		ValidatingWebhookConfigurationName: "spire-controller-manager-webhook",
	}

	options := ctrl.Options{Scheme: scheme}
	var ignoreNamespacesRegex []*regexp.Regexp

	if configFileFlag != "" {
		if err := spirev1alpha1.LoadOptionsFromFile(configFileFlag, scheme, &options, &ctrlConfig); err != nil {
			return ctrlConfig, options, ignoreNamespacesRegex, fmt.Errorf("unable to load the config file: %w", err)
		}

		for _, ignoredNamespace := range ctrlConfig.IgnoreNamespaces {
			regex, err := regexp.Compile(ignoredNamespace)
			if err != nil {
				return ctrlConfig, options, ignoreNamespacesRegex, fmt.Errorf("unable to compile ignore namespaces regex: %w", err)
			}

			ignoreNamespacesRegex = append(ignoreNamespacesRegex, regex)
		}
	}
	// Determine the SPIRE Server socket path
	switch {
	case ctrlConfig.SPIREServerSocketPath == "" && spireAPISocketFlag == "":
		// Neither is set. Use the default.
		ctrlConfig.SPIREServerSocketPath = defaultSPIREServerSocketPath
	case ctrlConfig.SPIREServerSocketPath != "" && spireAPISocketFlag == "":
		// Configuration file value is set. Use it.
	case ctrlConfig.SPIREServerSocketPath == "" && spireAPISocketFlag != "":
		// Deprecated flag value is set. Use it but warn.
		ctrlConfig.SPIREServerSocketPath = spireAPISocketFlag
		setupLog.Error(nil, "The spire-api-socket flag is deprecated and will be removed in a future release; use the configuration file instead")
	case ctrlConfig.SPIREServerSocketPath != "" && spireAPISocketFlag != "":
		// Both are set. Warn and ignore the deprecated flag.
		setupLog.Error(nil, "Ignoring deprecated spire-api-socket flag which will be removed in a future release")
	}

	// Attempt to auto detect cluster domain if it wasn't specified
	if ctrlConfig.ClusterDomain == "" {
		clusterDomain, err := autoDetectClusterDomain()
		if err != nil {
			setupLog.Error(err, "unable to autodetect cluster domain")
		}

		ctrlConfig.ClusterDomain = clusterDomain
	}

	setupLog.Info("Config loaded",
		"cluster name", ctrlConfig.ClusterName,
		"cluster domain", ctrlConfig.ClusterDomain,
		"trust domain", ctrlConfig.TrustDomain,
		"ignore namespaces", ctrlConfig.IgnoreNamespaces,
		"gc interval", ctrlConfig.GCInterval,
		"spire server socket path", ctrlConfig.SPIREServerSocketPath)

	switch {
	case ctrlConfig.TrustDomain == "":
		setupLog.Error(nil, "trust domain is required configuration")
		return ctrlConfig, options, ignoreNamespacesRegex, errors.New("trust domain is required configuration")
	case ctrlConfig.ClusterName == "":
		return ctrlConfig, options, ignoreNamespacesRegex, errors.New("cluster name is required configuration")
	case ctrlConfig.ValidatingWebhookConfigurationName == "":
		return ctrlConfig, options, ignoreNamespacesRegex, errors.New("validating webhook configuration name is required configuration")
	case ctrlConfig.ControllerManagerConfigurationSpec.Webhook.CertDir != "":
		setupLog.Info("certDir configuration is ignored", "certDir", ctrlConfig.ControllerManagerConfigurationSpec.Webhook.CertDir)
	}

	return ctrlConfig, options, ignoreNamespacesRegex, nil
}

func run(ctrlConfig spirev1alpha1.ControllerManagerConfig, options ctrl.Options, ignoreNamespacesRegex []*regexp.Regexp) error {
	// It's unfortunate that we have to keep credentials on disk so that the
	// manager can load them:
	// TODO: upstream a change to the WebhookServer so it can use callbacks to
	// obtain the certificates so we don't have to touch disk.
	certDir, err := os.MkdirTemp("", "spire-controller-manager-")
	if err != nil {
		setupLog.Error(err, "failed to create temporary cert directory")
		return err
	}
	defer func() {
		if err := os.RemoveAll(certDir); err != nil {
			setupLog.Error(err, "failed to remove temporary cert directory", "certDir", certDir)
			os.Exit(1)
		}
	}()

	// webhook server credentials are stored in a single file to keep rotation
	// simple.
	const keyPairName = "keypair.pem"
	options.WebhookServer = webhook.NewServer(webhook.Options{
		CertDir:  certDir,
		CertName: keyPairName,
		KeyName:  keyPairName,
		TLSOpts: []func(*tls.Config){
			func(s *tls.Config) {
				s.MinVersion = tls.VersionTLS12
			},
		},
	})

	ctx := ctrl.SetupSignalHandler()

	trustDomain, err := spiffeid.TrustDomainFromString(ctrlConfig.TrustDomain)
	if err != nil {
		setupLog.Error(err, "invalid trust domain name")
		return err
	}
	setupLog.Info("Dialing SPIRE Server socket")
	spireClient, err := spireapi.DialSocket(ctx, ctrlConfig.SPIREServerSocketPath)
	if err != nil {
		setupLog.Error(err, "unable to dial SPIRE Server socket")
		return err
	}
	defer spireClient.Close()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), options)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		return err
	}

	// We need a direct client to query and patch up the webhook. We can't use
	// the controller runtime client for this because we can't start the manager
	// without the webhook credentials being in place, and the webhook credentials
	// need the DNS name of the webhook service from the configuration.
	config, err := rest.InClusterConfig()
	if err != nil {
		setupLog.Error(err, "failed to get in cluster configuration")
		return err
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		setupLog.Error(err, "failed to create an API client")
		return err
	}

	_, resources, err := clientset.ServerGroupsAndResources()
	for _, r := range resources {
		if r.GroupVersion == "spire.spiffe.io/v1alpha1" {
			for _, k := range r.APIResources {
				setupLog.Info(fmt.Sprintf("checking kind %s", k.Kind))
				if k.Kind == "ClusterSPIFFEID" {
					setupLog.Info("Found ClusterSPIFFEID CRD")
					customResourcesPresent.ClusterSpiffeIDPresent = true
				}
				if k.Kind == "ClusterFederatedTrustDomain" {
					setupLog.Info("Found ClusterFederatedTrustDomain CRD")
					customResourcesPresent.ClusterFederatedTrustDomainPresent = true
				}
				if k.Kind == "ClusterStaticEntry" {
					setupLog.Info("Found ClusterStaticEntry CRD")
					customResourcesPresent.ClusterStaticEntryPresent = true
				}
			}
		}
	}

	if !customResourcesPresent.fullyInitialized() {
		setupLog.Info("CRDs missing watching for future creation of spire-controller-manager CRDs")
		dyn, err := dynamic.NewForConfig(config)
		if err != nil {
			return err
		}
		fac := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, time.Minute, metav1.NamespaceAll, nil)
		informer := fac.ForResource(schema.GroupVersionResource{
			Group:    apiextensions.GroupName,
			Version:  "v1",
			Resource: "customresourcedefinitions",
		}).Informer()

		informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				typedObj := obj.(*unstructured.Unstructured)
				bytes, _ := typedObj.MarshalJSON()

				crd := v1.CustomResourceDefinition{}
				json.Unmarshal(bytes, &crd)
				setupLog.Info(fmt.Sprintf("CRD added %+s", crd.Spec.Names.Kind))
				if crd.Spec.Names.Kind == "ClusterStaticEntry" {
					setupLog.Info("ClusterStaticEntry CRD added")
					customResourcesPresent.ClusterStaticEntryPresent = true
				} else if crd.Spec.Names.Kind == "ClusterFederatedTrustDomain" {
					setupLog.Info("ClusterFederatedTrustDomain CRD added")
					customResourcesPresent.ClusterFederatedTrustDomainPresent = true
				} else if crd.Spec.Names.Kind == "ClusterSPIFFEID" {
					setupLog.Info("ClusterSPIFFEID CRD added")
					customResourcesPresent.ClusterSpiffeIDPresent = true
				}

				if customResourcesPresent.fullyInitialized() {
					setupLog.Info("CRDs added restarting spire-manager-controller")
					os.Exit(0)
				}
			},
		})
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()

		go informer.Run(ctx.Done())
	}

	webhookID, _ := spiffeid.FromPath(trustDomain, "/spire-controller-manager-webhook")
	webhookManager := webhookmanager.New(webhookmanager.Config{
		ID:            webhookID,
		KeyPairPath:   filepath.Join(certDir, keyPairName),
		WebhookName:   ctrlConfig.ValidatingWebhookConfigurationName,
		WebhookClient: clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations(),
		SVIDClient:    spireClient,
		BundleClient:  spireClient,
	})

	if err := webhookManager.Init(ctx); err != nil {
		setupLog.Error(err, "failed to mint initial webhook certificate")
		return err
	}

	entryReconciler := spireentry.Reconciler(spireentry.ReconcilerConfig{
		TrustDomain:      trustDomain,
		ClusterName:      ctrlConfig.ClusterName,
		ClusterDomain:    ctrlConfig.ClusterDomain,
		K8sClient:        mgr.GetClient(),
		EntryClient:      spireClient,
		IgnoreNamespaces: ignoreNamespacesRegex,
		GCInterval:       ctrlConfig.GCInterval,
	})

	federationRelationshipReconciler := spirefederationrelationship.Reconciler(spirefederationrelationship.ReconcilerConfig{
		K8sClient:         mgr.GetClient(),
		TrustDomainClient: spireClient,
		GCInterval:        ctrlConfig.GCInterval,
	})

	if customResourcesPresent.ClusterSpiffeIDPresent {
		if err = (&controllers.ClusterSPIFFEIDReconciler{
			Client:    mgr.GetClient(),
			Scheme:    mgr.GetScheme(),
			Triggerer: entryReconciler,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "ClusterSPIFFEID")
			return err
		}
	} else {
		setupLog.Info("ClusterSPIFFEID CRD was not installed, please install spire-controller-manager CRDs")
		setupLog.Info("ClusterSPIFFEIDReconciler will not be started")
	}

	if customResourcesPresent.ClusterFederatedTrustDomainPresent {
		if err = (&controllers.ClusterFederatedTrustDomainReconciler{
			Client:    mgr.GetClient(),
			Scheme:    mgr.GetScheme(),
			Triggerer: federationRelationshipReconciler,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "ClusterFederatedTrustDomain")
			return err
		}
	} else {
		setupLog.Info("ClusterFederatedTrustDomain CRD was not installed, please install spire-controller-manager CRDs")
		setupLog.Info("ClusterFederatedTrustDomainReconciler will not be started")
	}

	if customResourcesPresent.ClusterStaticEntryPresent {
		if err = (&controllers.ClusterStaticEntryReconciler{
			Client:    mgr.GetClient(),
			Scheme:    mgr.GetScheme(),
			Triggerer: entryReconciler,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "ClusterStaticEntry")
			return err
		}
	} else {
		setupLog.Info("ClusterStaticEntry CRD was not installed, please install spire-controller-manager CRDs")
		setupLog.Info("ClusterStaticEntryReconciler will not be started")
	}
	if err = (&spirev1alpha1.ClusterFederatedTrustDomain{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "ClusterFederatedTrustDomain")
		return err
	}
	if err = (&spirev1alpha1.ClusterSPIFFEID{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "ClusterSPIFFEID")
		return err
	}
	//+kubebuilder:scaffold:builder

	if err = (&controllers.PodReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Triggerer:        entryReconciler,
		IgnoreNamespaces: ignoreNamespacesRegex,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Pod")
		return err
	}

	if err = mgr.Add(manager.RunnableFunc(entryReconciler.Run)); err != nil {
		setupLog.Error(err, "unable to manage entry reconciler")
		return err
	}

	if err = mgr.Add(manager.RunnableFunc(federationRelationshipReconciler.Run)); err != nil {
		setupLog.Error(err, "unable to manage federation relationship reconciler")
		return err
	}

	if err = mgr.Add(webhookManager); err != nil {
		setupLog.Error(err, "unable to manage federation relationship reconciler")
		return err
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		return err
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		return err
	}

	return nil
}

func autoDetectClusterDomain() (string, error) {
	cname, err := net.LookupCNAME(k8sDefaultService)
	if err != nil {
		return "", fmt.Errorf("unable to lookup CNAME: %w", err)
	}

	clusterDomain, err := parseClusterDomainCNAME(cname)
	if err != nil {
		return "", fmt.Errorf("unable to parse CNAME \"%s\": %w", cname, err)
	}

	return clusterDomain, nil
}

func parseClusterDomainCNAME(cname string) (string, error) {
	clusterDomain := strings.TrimPrefix(cname, k8sDefaultService+".")
	if clusterDomain == cname {
		return "", errors.New("CNAME did not have expected prefix")
	}

	// Trim off optional trailing dot
	clusterDomain = strings.TrimSuffix(clusterDomain, ".")
	if clusterDomain == "" {
		return "", errors.New("CNAME did not have a cluster domain")
	}

	return clusterDomain, nil
}
