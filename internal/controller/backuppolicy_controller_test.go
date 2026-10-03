package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	backupv1 "github.com/EquinoxWN/db-backup-operator/api/v1alpha1"
)

// fakeExec records commands and answers wal-g and psql with canned results.
type fakeExec struct {
	calls     [][]string
	pods      []string
	backupErr error
	psqlOut   string
	psqlErr   error
}

func (f *fakeExec) Exec(_ context.Context, _, pod, _ string, cmd []string) (string, error) {
	f.calls = append(f.calls, cmd)
	f.pods = append(f.pods, pod)
	if cmd[0] == "wal-g" {
		return "", f.backupErr
	}
	return f.psqlOut, f.psqlErr
}

var created = time.Date(2026, 10, 1, 0, 30, 0, 0, time.UTC)

const healthyArchiver = "42|0|00000001000000000000002A|2026-10-02 01:59:00+00|\n"

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := backupv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func policy(schedule string) *backupv1.BackupPolicy {
	return &backupv1.BackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-db", Namespace: "db", CreationTimestamp: metav1.Time{Time: created}, Generation: 1},
		Spec:       backupv1.BackupPolicySpec{Selector: map[string]string{"role": "primary"}, Schedule: schedule},
	}
}

func pod(name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "db", Labels: map[string]string{"role": "primary"}},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

type harness struct {
	r    *BackupPolicyReconciler
	exec *fakeExec
	now  time.Time
}

func newHarness(t *testing.T, objs ...client.Object) *harness {
	t.Helper()
	h := &harness{exec: &fakeExec{psqlOut: healthyArchiver}}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&backupv1.BackupPolicy{}).Build()
	h.r = &BackupPolicyReconciler{Client: c, Exec: h.exec, Now: func() time.Time { return h.now }}
	return h
}

func (h *harness) reconcile(t *testing.T) (ctrl.Result, *backupv1.BackupPolicy) {
	t.Helper()
	key := types.NamespacedName{Namespace: "db", Name: "orders-db"}
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var p backupv1.BackupPolicy
	if err := h.r.Get(context.Background(), key, &p); err != nil {
		t.Fatal(err)
	}
	return res, &p
}

func condition(p *backupv1.BackupPolicy, kind string) *metav1.Condition {
	return meta.FindStatusCondition(p.Status.Conditions, kind)
}

func TestWaitsUntilTheScheduleIsDue(t *testing.T) {
	h := newHarness(t, policy("0 2 * * *"), pod("pg-0", corev1.PodRunning))
	h.now = created.Add(time.Hour) // 01:30, next run 02:00
	res, p := h.reconcile(t)
	if len(h.exec.calls) != 0 {
		t.Fatalf("ran %v before the schedule was due", h.exec.calls)
	}
	if res.RequeueAfter != 30*time.Minute {
		t.Fatalf("requeue after %v, want 30m", res.RequeueAfter)
	}
	if c := condition(p, backupv1.ConditionReady); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Scheduled" {
		t.Fatalf("Ready condition = %+v", c)
	}
}

func TestRunsWalgBackupPushInThePrimaryWhenDue(t *testing.T) {
	h := newHarness(t, policy("0 2 * * *"), pod("pg-0", corev1.PodRunning), pod("pg-old", corev1.PodSucceeded))
	h.now = time.Date(2026, 10, 2, 2, 0, 5, 0, time.UTC)
	res, p := h.reconcile(t)
	want := []string{"wal-g", "backup-push", "/var/lib/postgresql/data"}
	if len(h.exec.calls) < 1 || strings.Join(h.exec.calls[0], " ") != strings.Join(want, " ") || h.exec.pods[0] != "pg-0" {
		t.Fatalf("calls = %v on %v, want %v on pg-0", h.exec.calls, h.exec.pods, want)
	}
	if p.Status.LastSuccessfulBackupTime == nil || len(p.Status.History) != 1 || p.Status.History[0].Result != "Succeeded" {
		t.Fatalf("status = %+v", p.Status)
	}
	if !p.Status.LastScheduleTime.Time.Equal(time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("lastScheduleTime = %v, want the scheduled 02:00, not the run time", p.Status.LastScheduleTime)
	}
	if c := condition(p, backupv1.ConditionBackupSucceeded); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("BackupSucceeded = %+v", c)
	}
	if res.RequeueAfter <= 23*time.Hour {
		t.Fatalf("requeue after %v, want about 24h", res.RequeueAfter)
	}
}

func TestAFailedBackupIsRecordedAndReported(t *testing.T) {
	h := newHarness(t, policy("0 2 * * *"), pod("pg-0", corev1.PodRunning))
	h.exec.backupErr = errors.New("wal-g: command terminated with exit code 1: " + strings.Repeat("x", 2000))
	h.now = time.Date(2026, 10, 2, 2, 0, 5, 0, time.UTC)
	_, p := h.reconcile(t)
	c := condition(p, backupv1.ConditionBackupSucceeded)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "BackupFailed" {
		t.Fatalf("BackupSucceeded = %+v", c)
	}
	if p.Status.LastSuccessfulBackupTime != nil || p.Status.History[0].Result != "Failed" {
		t.Fatalf("status = %+v", p.Status)
	}
	if len(c.Message) > maxMessage || len(p.Status.History[0].Message) > maxMessage {
		t.Fatal("error output must be truncated in status")
	}
}

func TestMissedRunsCollapseIntoOneBackup(t *testing.T) {
	h := newHarness(t, policy("0 * * * *"), pod("pg-0", corev1.PodRunning))
	h.now = created.Add(10 * time.Hour) // operator was down; 10 hourly runs missed
	_, p := h.reconcile(t)
	backups := 0
	for _, c := range h.exec.calls {
		if c[0] == "wal-g" {
			backups++
		}
	}
	if backups != 1 {
		t.Fatalf("ran %d backups, want 1", backups)
	}
	if !p.Status.LastScheduleTime.Time.Equal(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("lastScheduleTime = %v, want the latest missed run 10:00", p.Status.LastScheduleTime)
	}
	if _, _ = h.reconcile(t); len(h.exec.calls) != 2 { // one backup plus one psql query; nothing new
		t.Fatalf("second reconcile ran more commands: %v", h.exec.calls)
	}
}

func TestHistoryKeepsTheTenNewest(t *testing.T) {
	h := newHarness(t, policy("0 * * * *"), pod("pg-0", corev1.PodRunning))
	var p *backupv1.BackupPolicy
	for i := 1; i <= 12; i++ {
		h.now = created.Add(time.Duration(i)*time.Hour + time.Minute)
		_, p = h.reconcile(t)
	}
	if len(p.Status.History) != historyLimit {
		t.Fatalf("history has %d entries, want %d", len(p.Status.History), historyLimit)
	}
	if !p.Status.History[0].ScheduledTime.After(p.Status.History[1].ScheduledTime.Time) {
		t.Fatal("history must be newest first")
	}
}

func TestPrimaryMustBeExactlyOneRunningPod(t *testing.T) {
	for name, pods := range map[string][]client.Object{
		"none":      {pod("pg-0", corev1.PodPending)},
		"two":       {pod("pg-0", corev1.PodRunning), pod("pg-1", corev1.PodRunning)},
		"no labels": {},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, append([]client.Object{policy("0 2 * * *")}, pods...)...)
			h.now = time.Date(2026, 10, 2, 2, 0, 5, 0, time.UTC)
			res, p := h.reconcile(t)
			if len(h.exec.calls) != 0 {
				t.Fatalf("ran %v without a single primary", h.exec.calls)
			}
			if c := condition(p, backupv1.ConditionReady); c == nil || c.Reason != "PrimaryNotFound" {
				t.Fatalf("Ready = %+v", c)
			}
			if p.Status.LastScheduleTime != nil || res.RequeueAfter != retryAfter {
				t.Fatal("the run must stay due and be retried")
			}
		})
	}
}

func TestInvalidScheduleIsReportedNotRetried(t *testing.T) {
	h := newHarness(t, policy("every day"), pod("pg-0", corev1.PodRunning))
	h.now = created.Add(48 * time.Hour)
	res, p := h.reconcile(t)
	if c := condition(p, backupv1.ConditionReady); c == nil || c.Reason != "InvalidSchedule" {
		t.Fatalf("Ready = %+v", c)
	}
	if res.RequeueAfter != 0 || len(h.exec.calls) != 0 {
		t.Fatal("an invalid schedule must not run or requeue")
	}
}

func TestSuspendedPolicyDoesNothing(t *testing.T) {
	p0 := policy("0 2 * * *")
	p0.Spec.Suspend = true
	h := newHarness(t, p0, pod("pg-0", corev1.PodRunning))
	h.now = created.Add(48 * time.Hour)
	_, p := h.reconcile(t)
	if len(h.exec.calls) != 0 || condition(p, backupv1.ConditionReady).Reason != "Suspended" {
		t.Fatal("a suspended policy must not back up")
	}
}

func TestWALArchivingCondition(t *testing.T) {
	cases := map[string]struct {
		out, reason string
		err         error
		ok          bool
	}{
		"healthy":       {out: healthyArchiver, ok: true, reason: "Archiving"},
		"never":         {out: "0|0|||\n", reason: "NotArchiving"},
		"latest failed": {out: "7|2|000000010000000000000007|2026-10-02 01:00:00+00|2026-10-02 01:30:00+00\n", reason: "NotArchiving"},
		"query error":   {err: errors.New("psql: connection refused"), reason: "QueryFailed"},
		"garbage":       {out: "permission denied", reason: "QueryFailed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, policy("0 2 * * *"), pod("pg-0", corev1.PodRunning))
			h.exec.psqlOut, h.exec.psqlErr = tc.out, tc.err
			h.now = time.Date(2026, 10, 2, 2, 0, 5, 0, time.UTC)
			_, p := h.reconcile(t)
			c := condition(p, backupv1.ConditionWALArchiving)
			if c == nil || (c.Status == metav1.ConditionTrue) != tc.ok || c.Reason != tc.reason {
				t.Fatalf("WALArchiving = %+v", c)
			}
			psql := h.exec.calls[len(h.exec.calls)-1]
			if psql[0] != "psql" || psql[2] != "postgres" || psql[len(psql)-1] != archiverQuery {
				t.Fatalf("psql argv = %v", psql)
			}
		})
	}
}

func TestDeletedPolicyIsIgnored(t *testing.T) {
	h := newHarness(t)
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "db", Name: "gone"}})
	if err != nil || res != (ctrl.Result{}) {
		t.Fatalf("reconcile of a missing policy = %v, %v", res, err)
	}
}

func TestScheduleIsUTCWhateverTheLocalTimeZone(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("UTC+7", 7*3600) // the bug only shows when local time is not UTC
	t.Cleanup(func() { time.Local = saved })
	h := newHarness(t, policy("0 2 * * *"), pod("pg-0", corev1.PodRunning))
	h.now = time.Date(2026, 10, 2, 2, 0, 5, 0, time.UTC)
	_, p := h.reconcile(t)
	if got := p.Status.LastScheduleTime.UTC(); !got.Equal(time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("lastScheduleTime = %v, want 02:00 UTC", got)
	}
}
