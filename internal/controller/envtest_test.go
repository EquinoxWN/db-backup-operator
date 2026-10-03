package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	backupv1 "github.com/EquinoxWN/db-backup-operator/api/v1alpha1"
)

// startAPIServer runs a real kube-apiserver and etcd with the generated CRD installed.
func startAPIServer(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run `make envtest`")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() {
		// On Windows envtest cannot signal its child processes; run this suite on Linux, macOS or WSL.
		if err := env.Stop(); err != nil {
			t.Logf("stopping envtest: %v", err)
		}
	})
	c, err := client.New(cfg, client.Options{Scheme: newScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAPIServerAppliesDefaultsAndRejectsBadPolicies(t *testing.T) {
	c := startAPIServer(t)
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "db"}}); err != nil {
		t.Fatal(err)
	}
	ok := policy("0 2 * * *")
	ok.CreationTimestamp = metav1.Time{}
	if err := c.Create(ctx, ok); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	if ok.Spec.Container != "postgres" || ok.Spec.DataDir != "/var/lib/postgresql/data" || ok.Spec.PostgresUser != "postgres" {
		t.Fatalf("defaults not applied: %+v", ok.Spec)
	}

	bad := map[string]func(*backupv1.BackupPolicySpec){
		"empty selector":       func(s *backupv1.BackupPolicySpec) { s.Selector = map[string]string{} },
		"missing schedule":     func(s *backupv1.BackupPolicySpec) { s.Schedule = "" },
		"relative data dir":    func(s *backupv1.BackupPolicySpec) { s.DataDir = "data" },
		"shell in data dir":    func(s *backupv1.BackupPolicySpec) { s.DataDir = "/data; rm -rf /" },
		"bad container name":   func(s *backupv1.BackupPolicySpec) { s.Container = "Postgres DB" },
		"sql in postgres user": func(s *backupv1.BackupPolicySpec) { s.PostgresUser = "postgres; DROP TABLE x" },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			p := policy("0 2 * * *")
			p.Name, p.CreationTimestamp = "bad-"+strings.ReplaceAll(name, " ", "-"), metav1.Time{}
			mutate(&p.Spec)
			if err := c.Create(ctx, p); err == nil {
				t.Fatalf("API server accepted %s", name)
			}
		})
	}
}

func TestReconcilerWritesStatusThroughTheRealAPI(t *testing.T) {
	c := startAPIServer(t)
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "db"}}); err != nil {
		t.Fatal(err)
	}
	pg := pod("pg-0", corev1.PodRunning)
	pg.Spec.Containers = []corev1.Container{{Name: "postgres", Image: "postgres:17"}}
	if err := c.Create(ctx, pg); err != nil {
		t.Fatal(err)
	}
	pg.Status.Phase = corev1.PodRunning // no kubelet here, so mark it running by hand
	if err := c.Status().Update(ctx, pg); err != nil {
		t.Fatal(err)
	}
	p := policy("0 * * * *")
	p.CreationTimestamp = metav1.Time{}
	if err := c.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	exec := &fakeExec{psqlOut: healthyArchiver}
	r := &BackupPolicyReconciler{Client: c, Exec: exec, Now: func() time.Time { return time.Now().Add(2 * time.Hour) }}
	key := types.NamespacedName{Namespace: "db", Name: "orders-db"}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got backupv1.BackupPolicy
	if err := c.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if len(exec.calls) != 2 || exec.calls[0][0] != "wal-g" || exec.pods[0] != "pg-0" {
		t.Fatalf("commands = %v on %v", exec.calls, exec.pods)
	}
	if len(got.Status.History) != 1 || got.Status.LastSuccessfulBackupTime == nil || got.Status.WALArchive == nil {
		t.Fatalf("status not persisted: %+v", got.Status)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Fatalf("observedGeneration %d, generation %d", got.Status.ObservedGeneration, got.Generation)
	}
}

func TestSamplePolicyIsAccepted(t *testing.T) {
	c := startAPIServer(t)
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", "backuppolicy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var sample backupv1.BackupPolicy
	if err := yaml.UnmarshalStrict(raw, &sample); err != nil {
		t.Fatalf("sample does not match the API types: %v", err)
	}
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sample.Namespace}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &sample); err != nil {
		t.Fatalf("API server rejected the sample: %v", err)
	}
}
