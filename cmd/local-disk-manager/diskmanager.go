package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	"github.com/operator-framework/operator-sdk/pkg/log/zap"
	"github.com/operator-framework/operator-sdk/pkg/metrics"
	sdkVersion "github.com/operator-framework/operator-sdk/version"
	logr "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	v1alpha1 "github.com/hwameistor/hwameistor/pkg/apis/hwameistor/v1alpha1"
	"github.com/hwameistor/hwameistor/pkg/local-disk-manager/controller"
	csidriver "github.com/hwameistor/hwameistor/pkg/local-disk-manager/csi/driver"
	"github.com/hwameistor/hwameistor/pkg/local-disk-manager/disk"
	"github.com/hwameistor/hwameistor/pkg/local-disk-manager/smart"
	"github.com/hwameistor/hwameistor/pkg/local-storage/utils"
)

// Change below variables to serve metrics on different host or port.
var (
	metricsHost               = "0.0.0.0"
	metricsPort         int32 = 8383
	operatorMetricsPort int32 = 8686
	csiCfg              csidriver.Config
	logLevel            = flag.Int("v", 4 /*Log Info*/, "number for the log level verbosity")
)
var log = logf.Log.WithName("cmd")

func printVersion() {
	log.Info(fmt.Sprintf("Go Version: %s", runtime.Version()))
	log.Info(fmt.Sprintf("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH))
	log.Info(fmt.Sprintf("Version of operator-sdk: %v", sdkVersion.Version))
}

func main() {
	// Add the zap logger flag set to the CLI. The flag set must
	// be added before calling pflag.Parse().
	pflag.CommandLine.AddFlagSet(zap.FlagSet())

	registerCSIParams()

	// Add flags registered by imported packages (e.g. glog and
	// controller-runtime)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	pflag.Parse()

	flag.Parse()

	// Use a zap logr.Logger implementation. If none of the zap
	// flags are configured (or if the zap flag set is not being
	// used), this defaults to a production zap logger.
	//
	// The logger instantiated here can be changed to any logger
	// implementing the logr.Logger interface. This logger will
	// be propagated through the whole operator, generating
	// uniform and structured logs.
	logf.SetLogger(zap.Logger())

	printVersion()

	// Get a config to talk to the apiserver
	cfg, err := config.GetConfig()
	if err != nil {
		log.Error(err, "")
		os.Exit(1)
	}
	setupLogging()

	// Create Cluster Manager
	clusterMgr, err := newClusterManager(cfg)
	if err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	// Create Node Manager
	nodeMgr, err := newNodeManager(cfg)
	if err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	stopCh := signals.SetupSignalHandler()

	log.Info("starting monitor disk")
	go disk.NewController(nodeMgr).StartMonitor()

	log.Info("starting collect S.M.A.R.T")
	go smart.NewCollector().WithSyncPeriod(time.Hour * 6).StartTimerCollect(stopCh)

	if csiCfg.Enable {
		log.Info("starting Disk CSI Driver")
		go csidriver.NewDiskDriver(csiCfg).Run()
	}

	// Add the Metrics Service
	addMetrics(stopCh, cfg)

	// Start Cluster Controller
	go startClusterController(stopCh, clusterMgr)

	// Start Node Controller
	go startNodeController(stopCh, nodeMgr)
	select {
	case <-stopCh.Done():
		log.Info("Receive exit signal.")
		time.Sleep(3 * time.Second)
		os.Exit(1)
	}
}

func startClusterController(ctx context.Context, mgr manager.Manager) {
	runCluster := func(c context.Context) {
		log.Info("Starting the Cluster Cmd")
		// Start the Cmd
		if err := mgr.Start(ctx); err != nil {
			log.Error(err, "Failed to start Cluster Cmd")
			os.Exit(1)
		}
	}

	// Acquired leader lease before proceeding
	if err := utils.RunWithLease(utils.GetNamespace(), utils.GetPodName(), fmt.Sprintf("local-disk-manager-master"), runCluster); err != nil {
		log.Error(err, "Failed to init cluster lease election")
		os.Exit(1)
	}
}

func startNodeController(ctx context.Context, mgr manager.Manager) {
	log.Info("Starting the Node Cmd.")
	// Start the Cmd
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "Failed to start Node Cmd")
	}

	os.Exit(1)
}

// addMetrics will create the Services and Service Monitors to allow the operator export the metrics by using
// the Prometheus operator
func addMetrics(ctx context.Context, cfg *rest.Config) {
	// Get the namespace the operator is currently deployed in.
	operatorNs, err := k8sutil.GetOperatorNamespace()
	if err != nil {
		if errors.Is(err, k8sutil.ErrRunLocal) {
			log.Info("Skipping CR metrics server creation; not running in a cluster.")
			return
		}
	}

	//if err := serveCRMetrics(cfg, operatorNs); err != nil {
	//	log.Info("Could not generate and serve custom resource metrics", "error", err.Error())
	//}

	// Add to the below struct any other metrics ports you want to expose.
	servicePorts := []v1.ServicePort{
		{Port: metricsPort, Name: metrics.OperatorPortName, Protocol: v1.ProtocolTCP, TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: metricsPort}},
		{Port: operatorMetricsPort, Name: metrics.CRPortName, Protocol: v1.ProtocolTCP, TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: operatorMetricsPort}},
	}

	// Create Service object to expose the metrics port(s).
	service, err := metrics.CreateMetricsService(ctx, cfg, servicePorts)
	if err != nil {
		log.Info("Could not create metrics Service", "error", err.Error())
	}

	// CreateServiceMonitors will automatically create the prometheus-operator ServiceMonitor resources
	// necessary to configure Prometheus to scrape metrics from this operator.
	services := []*v1.Service{service}

	// The ServiceMonitor is created in the same namespace where the operator is deployed
	_, err = metrics.CreateServiceMonitors(cfg, operatorNs, services)
	if err != nil {
		log.Info("Could not create ServiceMonitor object", "error", err.Error())
		// If this operator is deployed to a cluster without the prometheus-operator running, it will return
		// ErrServiceMonitorNotPresent, which can be used to safely skip ServiceMonitor creation.
		if err == metrics.ErrServiceMonitorNotPresent {
			log.Info("Install prometheus-operator in your cluster to create ServiceMonitor objects", "error", err.Error())
		}
	}
}

// serveCRMetrics gets the Operator/CustomResource GVKs and generates metrics based on those types.
// It serves those metrics on "http://metricsHost:operatorMetricsPort".
//func serveCRMetrics(cfg *rest.Config, operatorNs string) error {
//	// The function below returns a list of filtered operator/CR specific GVKs. For more control, override the GVK list below
//	// with your own custom logic. Note that if you are adding third party API schemas, probably you will need to
//	// customize this implementation to avoid permissions issues.
//	filteredGVK, err := k8sutil.GetGVKsFromAddToScheme(apis.AddToScheme)
//	if err != nil {
//		return err
//	}
//
//	// The metrics will be generated from the namespaces which are returned here.
//	// NOTE that passing nil or an empty list of namespaces in GenerateAndServeCRMetrics will result in an error.
//	ns, err := kubemetrics.GetNamespacesForMetrics(operatorNs)
//	if err != nil {
//		return err
//	}
//
//	// Generate and serve custom resource specific metrics.
//	err = kubemetrics.GenerateAndServeCRMetrics(cfg, ns, filteredGVK, metricsHost, operatorMetricsPort)
//	if err != nil {
//		return err
//	}
//	return nil
//}

func setupLogging() {
	// parse log level(default level: info)
	var level logr.Level
	if *logLevel >= int(logr.TraceLevel) {
		level = logr.TraceLevel
	} else if *logLevel <= int(logr.PanicLevel) {
		level = logr.PanicLevel
	} else {
		level = logr.Level(*logLevel)
	}

	logr.SetLevel(level)
	logr.SetFormatter(&logr.JSONFormatter{
		CallerPrettyfier: func(f *runtime.Frame) (string, string) {
			s := strings.Split(f.Function, ".")
			funcName := s[len(s)-1]
			fileName := path.Base(f.File)
			return funcName, fmt.Sprintf("%s:%d", fileName, f.Line)
		}})
	logr.SetReportCaller(true)
}

func registerCSIParams() {
	flag.StringVar(&csiCfg.Endpoint, "endpoint", "unix://csi/csi.sock", "CSI endpoint")
	flag.StringVar(&csiCfg.DriverName, "drivername", "disk.hwameistor.io", "name of the csidriver")
	flag.StringVar(&csiCfg.NodeID, "nodeid", "", "node id")
	flag.BoolVar(&csiCfg.Enable, "csi-enable", false, "enable disk CSI Driver")

	(&csiCfg).VendorVersion = csidriver.VendorVersion
}

func newClusterManager(cfg *rest.Config) (manager.Manager, error) {
	// Set default manager options
	options := manager.Options{
		MetricsBindAddress: fmt.Sprintf("%s:%d", metricsHost, metricsPort),
	}

	// Create a new manager to provide shared dependencies and start components
	mgr, err := manager.New(cfg, options)
	if err != nil {
		return nil, err
	}

	log.Info("Registering Cluster Components.")
	// Setup Scheme for all resources
	if err := v1alpha1.AddToScheme(mgr.GetScheme()); err != nil {
		return nil, err
	}

	// Setup Cache for field index
	setIndexField(mgr.GetCache())

	// Setup all Controllers
	if err := controller.AddToManager(mgr); err != nil {
		return nil, err
	}

	return mgr, nil
}

func newNodeManager(cfg *rest.Config) (manager.Manager, error) {
	// Set default manager options
	options := manager.Options{
		MetricsBindAddress: "0",
	}

	// Create a new manager to provide shared dependencies and start components
	mgr, err := manager.New(cfg, options)
	if err != nil {
		return nil, err
	}

	log.Info("Registering Node Components.")
	// Setup Scheme for node resources
	if err := v1alpha1.AddToScheme(mgr.GetScheme()); err != nil {
		return nil, err
	}

	// Setup Cache for field index
	setIndexField(mgr.GetCache())

	// Setup node Controllers
	if err := controller.AddToNodeManager(mgr); err != nil {
		return nil, err
	}

	return mgr, nil
}

// setIndexField must be called after scheme has been added
func setIndexField(cache cache.Cache) {
	indexes := []struct {
		field string
		Func  func(client.Object) []string
	}{
		{
			field: "spec.nodeName",
			Func: func(obj client.Object) []string {
				return []string{obj.(*v1alpha1.LocalDisk).Spec.NodeName}
			},
		},
	}

	for _, index := range indexes {
		if err := cache.IndexField(context.Background(), &v1alpha1.LocalDisk{}, index.field, index.Func); err != nil {
			log.Error(err, "failed to setup index field %s", index.field)
			continue
		}
		log.Info("setup index field successfully", "field", index.field)
	}
}
