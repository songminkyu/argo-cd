package metrics

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/argoproj/argo-cd/v3/util/db/mocks"

	gitopsCache "github.com/argoproj/gitops-engine/pkg/cache"
	"github.com/argoproj/gitops-engine/pkg/sync/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/yaml"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	appclientset "github.com/argoproj/argo-cd/v3/pkg/client/clientset/versioned/fake"
	appinformer "github.com/argoproj/argo-cd/v3/pkg/client/informers/externalversions"
	applister "github.com/argoproj/argo-cd/v3/pkg/client/listers/application/v1alpha1"

	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const fakeApp = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
  labels:
    team-name: my-team
    team-bu: bu-id
    argoproj.io/cluster: test-cluster
spec:
  destination:
    namespace: dummy-namespace
    name: cluster1
  project: important-project
  source:
    path: some/path
    repoURL: https://github.com/argoproj/argocd-example-apps.git
status:
  sync:
    status: Synced
  health:
    status: Healthy
`

const fakeApp2 = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app-2
  namespace: argocd
  labels:
    team-name: my-team
    team-bu: bu-id
    argoproj.io/cluster: test-cluster
spec:
  destination:
    namespace: dummy-namespace
    name: cluster1
  project: important-project
  source:
    path: some/path
    repoURL: https://github.com/argoproj/argocd-example-apps.git
  syncPolicy:
    automated:
      selfHeal: false
      prune: true
status:
  sync:
    status: Synced
  health:
    status: Healthy
operation:
  sync:
    dryRun: true
    revision: 041eab7439ece92c99b043f0e171788185b8fc1d
    syncStrategy:
      hook: {}
`

const fakeApp3 = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app-3
  namespace: argocd
  deletionTimestamp: "2020-03-16T09:17:45Z"
  labels:
    team-name: my-team
    team-bu: bu-id
    argoproj.io/cluster: test-cluster
spec:
  destination:
    namespace: dummy-namespace
    name: cluster1
  project: important-project
  source:
    path: some/path
    repoURL: https://github.com/argoproj/argocd-example-apps.git
  syncPolicy:
    automated:
      selfHeal: true
      prune: false
status:
  sync:
    status: OutOfSync
  health:
    status: Degraded
`

const fakeApp4 = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app-4
  namespace: argocd
  labels:
    team-name: my-team
    team-bu: bu-id
    argoproj.io/cluster: test-cluster
spec:
  destination:
    namespace: dummy-namespace
    name: cluster1
  project: important-project
  source:
    path: some/path
    repoURL: https://github.com/argoproj/argocd-example-apps.git
status:
  sync:
    status: OutOfSync
  health:
    status: Degraded
  conditions:
  - lastTransitionTime: "2024-08-07T12:25:40Z"
    message: Application has 1 orphaned resources
    type: OrphanedResourceWarning
  - lastTransitionTime: "2024-08-07T12:25:40Z"
    message: Resource Pod standalone-pod is excluded in the settings
    type: ExcludedResourceWarning
  - lastTransitionTime: "2024-08-07T12:25:40Z"
    message: Resource Endpoint raw-endpoint is excluded in the settings
    type: ExcludedResourceWarning
`

const fakeDefaultApp = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  destination:
    namespace: dummy-namespace
    name: cluster1
  source:
    path: some/path
    repoURL: https://github.com/argoproj/argocd-example-apps.git
status:
  sync:
    status: Synced
  health:
    status: Healthy
`

const fakeAppOperationRunning = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
  labels:
    team-name: my-team
    team-bu: bu-id
    argoproj.io/cluster: test-cluster
spec:
  destination:
    namespace: dummy-namespace
    name: cluster1
  project: important-project
  source:
    path: some/path
    repoURL: https://github.com/argoproj/argocd-example-apps.git
status:
  sync:
    status: OutOfSync
  health:
    status: Progressing
  operationState:
    phase: Running
    startedAt: "2025-01-29T08:42:34Z"
`

const fakeAppOperationFinished = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
  labels:
    team-name: my-team
    team-bu: bu-id
    argoproj.io/cluster: test-cluster
spec:
  destination:
    namespace: dummy-namespace
    name: cluster1
  project: important-project
  source:
    path: some/path
    repoURL: https://github.com/argoproj/argocd-example-apps.git
status:
  sync:
    status: Synced
  health:
    status: Healthy
  operationState:
    phase: Succeeded
    startedAt: "2025-01-29T08:42:34Z"
    finishedAt: "2025-01-29T08:42:35Z"
`

var noOpHealthCheck = func(_ *http.Request) error {
	return nil
}

var appFilter = func(_ any) bool {
	return true
}

func init() {
	// Create a fake manager
	mgr, err := manager.New(&rest.Config{}, manager.Options{})
	if err != nil {
		panic(err)
	}
	// Create a fake controller so we initialize the internal controller metrics.
	// https://github.com/kubernetes-sigs/controller-runtime/blob/4000e996a202917ad7d40f02ed8a2079a9ce25e9/pkg/internal/controller/metrics/metrics.go
	_, _ = controller.New("test-controller", mgr, controller.Options{})
}

func newFakeApp(fakeAppYAML string) *argoappv1.Application {
	var app argoappv1.Application
	err := yaml.Unmarshal([]byte(fakeAppYAML), &app)
	if err != nil {
		panic(err)
	}
	return &app
}

func newFakeLister(fakeAppYAMLs ...string) (context.CancelFunc, applister.ApplicationLister) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var fakeApps []runtime.Object
	for _, appYAML := range fakeAppYAMLs {
		a := newFakeApp(appYAML)
		fakeApps = append(fakeApps, a)
	}
	appClientset := appclientset.NewSimpleClientset(fakeApps...)
	factory := appinformer.NewSharedInformerFactoryWithOptions(appClientset, 0, appinformer.WithNamespace("argocd"), appinformer.WithTweakListOptions(func(_ *metav1.ListOptions) {}))
	appInformer := factory.Argoproj().V1alpha1().Applications().Informer()
	go appInformer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), appInformer.HasSynced) {
		log.Fatal("Timed out waiting for caches to sync")
	}
	return cancel, factory.Argoproj().V1alpha1().Applications().Lister()
}

func testApp(t *testing.T, fakeAppYAMLs []string, expectedResponse string) {
	t.Helper()
	testMetricServer(t, fakeAppYAMLs, expectedResponse, []string{}, []string{})
}

type fakeClusterInfo struct {
	clustersInfo []gitopsCache.ClusterInfo
}

func (f *fakeClusterInfo) GetClustersInfo() []gitopsCache.ClusterInfo {
	return f.clustersInfo
}

type TestMetricServerConfig struct {
	FakeAppYAMLs     []string
	ExpectedResponse string
	AppLabels        []string
	AppConditions    []string
	ClusterLabels    []string
	ClustersInfo     []gitopsCache.ClusterInfo
	ClusterLister    ClusterLister
}

func testMetricServer(t *testing.T, fakeAppYAMLs []string, expectedResponse string, appLabels []string, appConditions []string) {
	t.Helper()
	cfg := TestMetricServerConfig{
		FakeAppYAMLs:     fakeAppYAMLs,
		ExpectedResponse: expectedResponse,
		AppLabels:        appLabels,
		AppConditions:    appConditions,
		ClusterLabels:    []string{},
		ClustersInfo:     []gitopsCache.ClusterInfo{},
	}
	runTest(t, cfg)
}

func runTest(t *testing.T, cfg TestMetricServerConfig) {
	t.Helper()
	cancel, appLister := newFakeLister(cfg.FakeAppYAMLs...)
	defer cancel()
	mockDB := mocks.NewArgoDB(t)
	mockDB.On("GetClusterServersByName", mock.Anything, "cluster1").Return([]string{"https://localhost:6443"}, nil)
	mockDB.On("GetCluster", mock.Anything, "https://localhost:6443").Return(&argoappv1.Cluster{Name: "cluster1", Server: "https://localhost:6443"}, nil)
	metricsServ, err := NewMetricsServer("localhost:8082", appLister, appFilter, noOpHealthCheck, cfg.AppLabels, cfg.AppConditions, mockDB)
	require.NoError(t, err)

	if len(cfg.ClustersInfo) > 0 {
		ci := &fakeClusterInfo{clustersInfo: cfg.ClustersInfo}
		collector := NewClusterCollector(t.Context(), ci, cfg.ClusterLister, cfg.ClusterLabels)
		metricsServ.registry.MustRegister(collector)
	}

	req, err := http.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	metricsServ.Handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assertMetricsPrinted(t, cfg.ExpectedResponse, body)
}

type testCombination struct {
	applications     []string
	responseContains string
}

func TestMetrics(t *testing.T) {
	combinations := []testCombination{
		{
			applications: []string{fakeApp, fakeApp2, fakeApp3},
			responseContains: `
# HELP argocd_app_info Information about application.
# TYPE argocd_app_info gauge
argocd_app_info{autosync_enabled="true",dest_namespace="dummy-namespace",dest_server="https://localhost:6443",health_status="Degraded",name="my-app-3",namespace="argocd",operation="delete",project="important-project",repo="https://github.com/argoproj/argocd-example-apps",sync_status="OutOfSync"} 1
argocd_app_info{autosync_enabled="false",dest_namespace="dummy-namespace",dest_server="https://localhost:6443",health_status="Healthy",name="my-app",namespace="argocd",operation="",project="important-project",repo="https://github.com/argoproj/argocd-example-apps",sync_status="Synced"} 1
argocd_app_info{autosync_enabled="true",dest_namespace="dummy-namespace",dest_server="https://localhost:6443",health_status="Healthy",name="my-app-2",namespace="argocd",operation="sync",project="important-project",repo="https://github.com/argoproj/argocd-example-apps",sync_status="Synced"} 1
`,
		},
		{
			applications: []string{fakeDefaultApp},
			responseContains: `
# HELP argocd_app_info Information about application.
# TYPE argocd_app_info gauge
argocd_app_info{autosync_enabled="false",dest_namespace="dummy-namespace",dest_server="https://localhost:6443",health_status="Healthy",name="my-app",namespace="argocd",operation="",project="default",repo="https://github.com/argoproj/argocd-example-apps",sync_status="Synced"} 1
`,
		},
	}

	for _, combination := range combinations {
		testApp(t, combination.applications, combination.responseContains)
	}
}

func TestMetricLabels(t *testing.T) {
	type testCases struct {
		testCombination
		description  string
		metricLabels []string
	}
	cases := []testCases{
		{
			description:  "will return the labels metrics successfully",
			metricLabels: []string{"team-name", "team-bu", "argoproj.io/cluster"},
			testCombination: testCombination{
				applications: []string{fakeApp, fakeApp2, fakeApp3},
				responseContains: `
# TYPE argocd_app_labels gauge
argocd_app_labels{label_argoproj_io_cluster="test-cluster",label_team_bu="bu-id",label_team_name="my-team",name="my-app",namespace="argocd",project="important-project"} 1
argocd_app_labels{label_argoproj_io_cluster="test-cluster",label_team_bu="bu-id",label_team_name="my-team",name="my-app-2",namespace="argocd",project="important-project"} 1
argocd_app_labels{label_argoproj_io_cluster="test-cluster",label_team_bu="bu-id",label_team_name="my-team",name="my-app-3",namespace="argocd",project="important-project"} 1
`,
			},
		},
		{
			description:  "metric will have empty label value if not present in the application",
			metricLabels: []string{"non-existing"},
			testCombination: testCombination{
				applications: []string{fakeApp, fakeApp2, fakeApp3},
				responseContains: `
# TYPE argocd_app_labels gauge
argocd_app_labels{label_non_existing="",name="my-app",namespace="argocd",project="important-project"} 1
argocd_app_labels{label_non_existing="",name="my-app-2",namespace="argocd",project="important-project"} 1
argocd_app_labels{label_non_existing="",name="my-app-3",namespace="argocd",project="important-project"} 1
`,
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.description, func(t *testing.T) {
			testMetricServer(t, c.applications, c.responseContains, c.metricLabels, []string{})
		})
	}
}

func TestMetricConditions(t *testing.T) {
	type testCases struct {
		testCombination
		description      string
		metricConditions []string
	}
	cases := []testCases{
		{
			description:      "metric will only output OrphanedResourceWarning",
			metricConditions: []string{"OrphanedResourceWarning"},
			testCombination: testCombination{
				applications: []string{fakeApp4},
				responseContains: `
# HELP argocd_app_condition Report application conditions.
# TYPE argocd_app_condition gauge
argocd_app_condition{condition="OrphanedResourceWarning",name="my-app-4",namespace="argocd",project="important-project"} 1
`,
			},
		},
		{
			description:      "metric will only output ExcludedResourceWarning",
			metricConditions: []string{"ExcludedResourceWarning"},
			testCombination: testCombination{
				applications: []string{fakeApp4},
				responseContains: `
# HELP argocd_app_condition Report application conditions.
# TYPE argocd_app_condition gauge
argocd_app_condition{condition="ExcludedResourceWarning",name="my-app-4",namespace="argocd",project="important-project"} 2
`,
			},
		},
		{
			description:      "metric will only output both OrphanedResourceWarning and ExcludedResourceWarning",
			metricConditions: []string{"ExcludedResourceWarning", "OrphanedResourceWarning"},
			testCombination: testCombination{
				applications: []string{fakeApp4},
				responseContains: `
# HELP argocd_app_condition Report application conditions.
# TYPE argocd_app_condition gauge
argocd_app_condition{condition="OrphanedResourceWarning",name="my-app-4",namespace="argocd",project="important-project"} 1
argocd_app_condition{condition="ExcludedResourceWarning",name="my-app-4",namespace="argocd",project="important-project"} 2
`,
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.description, func(t *testing.T) {
			testMetricServer(t, c.applications, c.responseContains, []string{}, c.metricConditions)
		})
	}
}

func TestMetricsSyncCounter(t *testing.T) {
	cancel, appLister := newFakeLister()
	defer cancel()
	mockDB := mocks.NewArgoDB(t)
	metricsServ, err := NewMetricsServer("localhost:8082", appLister, appFilter, noOpHealthCheck, []string{}, []string{}, mockDB)
	require.NoError(t, err)

	appSyncTotal := `
# HELP argocd_app_sync_total Number of application syncs.
# TYPE argocd_app_sync_total counter
argocd_app_sync_total{dest_server="https://localhost:6443",dry_run="false",name="my-app",namespace="argocd",phase="Error",project="important-project"} 1
argocd_app_sync_total{dest_server="https://localhost:6443",dry_run="false",name="my-app",namespace="argocd",phase="Failed",project="important-project"} 1
argocd_app_sync_total{dest_server="https://localhost:6443",dry_run="false",name="my-app",namespace="argocd",phase="Succeeded",project="important-project"} 2
`

	fakeApp := newFakeApp(fakeApp)
	metricsServ.IncSync(fakeApp, "https://localhost:6443", &argoappv1.OperationState{Phase: common.OperationRunning})
	metricsServ.IncSync(fakeApp, "https://localhost:6443", &argoappv1.OperationState{Phase: common.OperationFailed})
	metricsServ.IncSync(fakeApp, "https://localhost:6443", &argoappv1.OperationState{Phase: common.OperationError})
	metricsServ.IncSync(fakeApp, "https://localhost:6443", &argoappv1.OperationState{Phase: common.OperationSucceeded})
	metricsServ.IncSync(fakeApp, "https://localhost:6443", &argoappv1.OperationState{Phase: common.OperationSucceeded})

	req, err := http.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	metricsServ.Handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	log.Println(body)
	assertMetricsPrinted(t, appSyncTotal, body)
}

// assertMetricsPrinted asserts every line in the expected lines appears in the body
func assertMetricsPrinted(t *testing.T, expectedLines, body string) {
	t.Helper()
	for _, line := range strings.Split(expectedLines, "\n") {
		if line == "" {
			continue
		}
		assert.Contains(t, body, line, "expected metrics mismatch for line: %s", line)
	}
}

// assertMetricsNotPrinted
func assertMetricsNotPrinted(t *testing.T, expectedLines, body string) {
	t.Helper()
	for _, line := range strings.Split(expectedLines, "\n") {
		if line == "" {
			continue
		}
		assert.NotContains(t, body, expectedLines)
	}
}

func TestMetricsSyncDuration(t *testing.T) {
	cancel, appLister := newFakeLister()
	defer cancel()
	mockDB := mocks.NewArgoDB(t)
	metricsServ, err := NewMetricsServer("localhost:8082", appLister, appFilter, noOpHealthCheck, []string{}, []string{}, mockDB)
	require.NoError(t, err)

	t.Run("metric is not generated during Operation Running.", func(t *testing.T) {
		fakeAppOperationRunning := newFakeApp(fakeAppOperationRunning)
		metricsServ.IncAppSyncDuration(fakeAppOperationRunning, "https://localhost:6443", fakeAppOperationRunning.Status.OperationState)

		req, err := http.NewRequest(http.MethodGet, "/metrics", http.NoBody)
		require.NoError(t, err)
		rr := httptest.NewRecorder()
		metricsServ.Handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
		body := rr.Body.String()
		log.Println(body)
		assertMetricsNotPrinted(t, "argocd_app_sync_duration_seconds_total", body)
	})

	t.Run("metric is created when Operation Finished.", func(t *testing.T) {
		fakeAppOperationFinished := newFakeApp(fakeAppOperationFinished)
		metricsServ.IncAppSyncDuration(fakeAppOperationFinished, "https://localhost:6443", fakeAppOperationFinished.Status.OperationState)

		req, err := http.NewRequest(http.MethodGet, "/metrics", http.NoBody)
		require.NoError(t, err)
		rr := httptest.NewRecorder()
		metricsServ.Handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
		body := rr.Body.String()
		appSyncDurationTotal := `
# HELP argocd_app_sync_duration_seconds_total Application sync performance in seconds total.
# TYPE argocd_app_sync_duration_seconds_total counter
argocd_app_sync_duration_seconds_total{dest_server="https://localhost:6443",name="my-app",namespace="argocd",project="important-project"} 1
`
		log.Println(body)
		assertMetricsPrinted(t, appSyncDurationTotal, body)
	})
}

func TestReconcileMetrics(t *testing.T) {
	cancel, appLister := newFakeLister()
	defer cancel()
	mockDB := mocks.NewArgoDB(t)
	metricsServ, err := NewMetricsServer("localhost:8082", appLister, appFilter, noOpHealthCheck, []string{}, []string{}, mockDB)
	require.NoError(t, err)

	appReconcileMetrics := `
# HELP argocd_app_reconcile Application reconciliation performance in seconds.
# TYPE argocd_app_reconcile histogram
argocd_app_reconcile_bucket{dest_server="https://localhost:6443",namespace="argocd",le="0.25"} 0
argocd_app_reconcile_bucket{dest_server="https://localhost:6443",namespace="argocd",le="0.5"} 0
argocd_app_reconcile_bucket{dest_server="https://localhost:6443",namespace="argocd",le="1"} 0
argocd_app_reconcile_bucket{dest_server="https://localhost:6443",namespace="argocd",le="2"} 0
argocd_app_reconcile_bucket{dest_server="https://localhost:6443",namespace="argocd",le="4"} 0
argocd_app_reconcile_bucket{dest_server="https://localhost:6443",namespace="argocd",le="8"} 1
argocd_app_reconcile_bucket{dest_server="https://localhost:6443",namespace="argocd",le="16"} 1
argocd_app_reconcile_bucket{dest_server="https://localhost:6443",namespace="argocd",le="+Inf"} 1
argocd_app_reconcile_sum{dest_server="https://localhost:6443",namespace="argocd"} 5
argocd_app_reconcile_count{dest_server="https://localhost:6443",namespace="argocd"} 1
`
	fakeApp := newFakeApp(fakeApp)
	metricsServ.IncReconcile(fakeApp, "https://localhost:6443", 5*time.Second)

	req, err := http.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	metricsServ.Handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	log.Println(body)
	assertMetricsPrinted(t, appReconcileMetrics, body)
}

func TestOrphanedResourcesMetric(t *testing.T) {
	cancel, appLister := newFakeLister()
	defer cancel()
	mockDB := mocks.NewArgoDB(t)
	metricsServ, err := NewMetricsServer("localhost:8082", appLister, appFilter, noOpHealthCheck, []string{}, []string{}, mockDB)
	require.NoError(t, err)

	expectedMetrics := `
# HELP argocd_app_orphaned_resources_count Number of orphaned resources per application
# TYPE argocd_app_orphaned_resources_count gauge
argocd_app_orphaned_resources_count{name="my-app-4",namespace="argocd",project="important-project"} 1
`
	app := newFakeApp(fakeApp4)
	numOrphanedResources := 1
	metricsServ.SetOrphanedResourcesMetric(app, numOrphanedResources)

	req, err := http.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	metricsServ.Handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	log.Println(body)
	assertMetricsPrinted(t, expectedMetrics, body)
}

func TestMetricsReset(t *testing.T) {
	cancel, appLister := newFakeLister()
	defer cancel()
	mockDB := mocks.NewArgoDB(t)
	metricsServ, err := NewMetricsServer("localhost:8082", appLister, appFilter, noOpHealthCheck, []string{}, []string{}, mockDB)
	require.NoError(t, err)

	appSyncTotal := `
# HELP argocd_app_sync_total Number of application syncs.
# TYPE argocd_app_sync_total counter
argocd_app_sync_total{dest_server="https://localhost:6443",dry_run="false",name="my-app",namespace="argocd",phase="Error",project="important-project"} 1
argocd_app_sync_total{dest_server="https://localhost:6443",dry_run="false",name="my-app",namespace="argocd",phase="Failed",project="important-project"} 1
argocd_app_sync_total{dest_server="https://localhost:6443",dry_run="false",name="my-app",namespace="argocd",phase="Succeeded",project="important-project"} 2
`

	req, err := http.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	metricsServ.Handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assertMetricsPrinted(t, appSyncTotal, body)

	err = metricsServ.SetExpiration(time.Second)
	require.NoError(t, err)
	time.Sleep(2 * time.Second)
	req, err = http.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	require.NoError(t, err)
	rr = httptest.NewRecorder()
	metricsServ.Handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	body = rr.Body.String()
	log.Println(body)
	assertMetricsNotPrinted(t, appSyncTotal, body)
	err = metricsServ.SetExpiration(time.Second)
	require.Error(t, err)
}

func TestWorkqueueMetrics(t *testing.T) {
	cancel, appLister := newFakeLister()
	defer cancel()
	mockDB := mocks.NewArgoDB(t)
	metricsServ, err := NewMetricsServer("localhost:8082", appLister, appFilter, noOpHealthCheck, []string{}, []string{}, mockDB)
	require.NoError(t, err)

	expectedMetrics := `
# TYPE workqueue_adds_total counter
workqueue_adds_total{controller="test",name="test"}
# TYPE workqueue_depth gauge
workqueue_depth{controller="test",name="test",priority=""}
# TYPE workqueue_longest_running_processor_seconds gauge
workqueue_longest_running_processor_seconds{controller="test",name="test"}
# TYPE workqueue_queue_duration_seconds histogram
# TYPE workqueue_unfinished_work_seconds gauge
workqueue_unfinished_work_seconds{controller="test",name="test"}
# TYPE workqueue_work_duration_seconds histogram
`
	workqueue.NewNamed("test")

	req, err := http.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	metricsServ.Handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	log.Println(body)
	assertMetricsPrinted(t, expectedMetrics, body)
}

func TestGoMetrics(t *testing.T) {
	cancel, appLister := newFakeLister()
	defer cancel()
	mockDB := mocks.NewArgoDB(t)
	metricsServ, err := NewMetricsServer("localhost:8082", appLister, appFilter, noOpHealthCheck, []string{}, []string{}, mockDB)
	require.NoError(t, err)

	expectedMetrics := `
# TYPE go_gc_duration_seconds summary
go_gc_duration_seconds_sum
go_gc_duration_seconds_count
# TYPE go_goroutines gauge
go_goroutines
# TYPE go_info gauge
go_info
# TYPE go_memstats_alloc_bytes gauge
go_memstats_alloc_bytes
# TYPE go_memstats_sys_bytes gauge
go_memstats_sys_bytes
# TYPE go_threads gauge
go_threads
`

	req, err := http.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	metricsServ.Handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	log.Println(body)
	assertMetricsPrinted(t, expectedMetrics, body)
}
