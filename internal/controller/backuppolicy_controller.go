// Package controller reconciles BackupPolicy objects.
package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backupv1 "github.com/EquinoxWN/db-backup-operator/api/v1alpha1"
)

const (
	historyLimit  = 10
	backupTimeout = 2 * time.Hour
	queryTimeout  = 30 * time.Second
	retryAfter    = time.Minute
	maxMessage    = 512
)

// Executor runs a command in a container of a pod and returns its standard output.
type Executor interface {
	Exec(ctx context.Context, namespace, pod, container string, command []string) (string, error)
}

// BackupPolicyReconciler runs WAL-G base backups on schedule and checks WAL archiving.
type BackupPolicyReconciler struct {
	client.Client
	Exec Executor
	Now  func() time.Time
}

// +kubebuilder:rbac:groups=dbbackup.equinoxwn.github.io,resources=backuppolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=dbbackup.equinoxwn.github.io,resources=backuppolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create

// Reconcile brings one policy up to date and requeues for its next scheduled run.
func (r *BackupPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var p backupv1.BackupPolicy
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	defaults(&p.Spec)
	p.Status.ObservedGeneration = p.Generation

	sched, err := cron.ParseStandard(p.Spec.Schedule)
	if err != nil {
		setCondition(&p, backupv1.ConditionReady, false, "InvalidSchedule", err.Error())
		return ctrl.Result{}, r.Status().Update(ctx, &p) // nothing to retry until the spec changes
	}
	if p.Spec.Suspend {
		p.Status.NextScheduleTime = nil
		setCondition(&p, backupv1.ConditionReady, false, "Suspended", "spec.suspend is true")
		return ctrl.Result{}, r.Status().Update(ctx, &p)
	}

	now := r.Now().UTC()
	last := p.CreationTimestamp.Time
	if p.Status.LastScheduleTime != nil {
		last = p.Status.LastScheduleTime.Time
	}
	due, next := dueTime(sched, last, now)
	if due.IsZero() {
		p.Status.NextScheduleTime = &metav1.Time{Time: next}
		setCondition(&p, backupv1.ConditionReady, true, "Scheduled", "next backup at "+next.Format(time.RFC3339))
		return ctrl.Result{RequeueAfter: next.Sub(now)}, r.Status().Update(ctx, &p)
	}

	pod, err := r.primaryPod(ctx, &p)
	if err != nil {
		setCondition(&p, backupv1.ConditionReady, false, "PrimaryNotFound", err.Error())
		return ctrl.Result{RequeueAfter: retryAfter}, r.Status().Update(ctx, &p) // schedule not advanced: retried
	}

	log.Info("starting base backup", "pod", pod, "scheduled", due)
	r.backup(ctx, &p, pod, due)
	r.checkArchiving(ctx, &p, pod)
	p.Status.LastScheduleTime = &metav1.Time{Time: due}
	p.Status.NextScheduleTime = &metav1.Time{Time: next}
	setCondition(&p, backupv1.ConditionReady, true, "Scheduled", "next backup at "+next.Format(time.RFC3339))
	if err := r.Status().Update(ctx, &p); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: max(next.Sub(r.Now().UTC()), time.Second)}, nil
}

// backup runs wal-g backup-push and records the outcome in status.
func (r *BackupPolicyReconciler) backup(ctx context.Context, p *backupv1.BackupPolicy, pod string, due time.Time) {
	start := r.Now().UTC()
	runCtx, cancel := context.WithTimeout(ctx, backupTimeout)
	defer cancel()
	// argv, not a shell string: no field of the policy can inject a command.
	_, err := r.Exec.Exec(runCtx, p.Namespace, pod, p.Spec.Container, []string{"wal-g", "backup-push", p.Spec.DataDir})
	rec := backupv1.BackupRecord{
		ScheduledTime:  metav1.Time{Time: due},
		StartTime:      metav1.Time{Time: start},
		CompletionTime: metav1.Time{Time: r.Now().UTC()},
		Result:         "Succeeded",
	}
	if err != nil {
		rec.Result, rec.Message = "Failed", truncate(err.Error())
		setCondition(p, backupv1.ConditionBackupSucceeded, false, "BackupFailed", rec.Message)
	} else {
		p.Status.LastSuccessfulBackupTime = &rec.CompletionTime
		setCondition(p, backupv1.ConditionBackupSucceeded, true, "BackupSucceeded", "base backup completed at "+rec.CompletionTime.Format(time.RFC3339))
	}
	p.Status.History = append([]backupv1.BackupRecord{rec}, p.Status.History...)
	if len(p.Status.History) > historyLimit {
		p.Status.History = p.Status.History[:historyLimit]
	}
}

// checkArchiving reads pg_stat_archiver and sets the WALArchiving condition.
func (r *BackupPolicyReconciler) checkArchiving(ctx context.Context, p *backupv1.BackupPolicy, pod string) {
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	out, err := r.Exec.Exec(qctx, p.Namespace, pod, p.Spec.Container,
		[]string{"psql", "-U", p.Spec.PostgresUser, "-X", "-A", "-t", "-c", archiverQuery})
	if err != nil {
		setCondition(p, backupv1.ConditionWALArchiving, false, "QueryFailed", truncate(err.Error()))
		return
	}
	status, err := parseArchiver(out)
	if err != nil {
		setCondition(p, backupv1.ConditionWALArchiving, false, "QueryFailed", truncate(err.Error()))
		return
	}
	p.Status.WALArchive = status
	ok, msg := archivingHealthy(status)
	reason := "Archiving"
	if !ok {
		reason = "NotArchiving"
	}
	setCondition(p, backupv1.ConditionWALArchiving, ok, reason, msg)
}

// primaryPod returns the single running pod matched by the selector.
func (r *BackupPolicyReconciler) primaryPod(ctx context.Context, p *backupv1.BackupPolicy) (string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(p.Namespace), client.MatchingLabels(p.Spec.Selector)); err != nil {
		return "", err
	}
	var running []string
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil {
			running = append(running, pod.Name)
		}
	}
	if len(running) != 1 {
		return "", fmt.Errorf("selector must match exactly one running pod, found %d", len(running))
	}
	return running[0], nil
}

// dueTime returns the most recent missed run (zero if none) and the next run after now.
func dueTime(sched cron.Schedule, last, now time.Time) (due, next time.Time) {
	last, now = last.UTC(), now.UTC() // cron follows the input's zone; the API promises UTC
	for t := sched.Next(last); !t.After(now); t = sched.Next(t) {
		due = t // runs missed while the operator was down collapse into the latest one
	}
	return due, sched.Next(now)
}

// defaults fills optional fields the API server would default.
func defaults(s *backupv1.BackupPolicySpec) {
	if s.Container == "" {
		s.Container = "postgres"
	}
	if s.PostgresUser == "" {
		s.PostgresUser = "postgres"
	}
	if s.DataDir == "" {
		s.DataDir = "/var/lib/postgresql/data"
	}
}

// setCondition records one condition on the policy.
func setCondition(p *backupv1.BackupPolicy, kind string, ok bool, reason, message string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: kind, Status: status, Reason: reason, Message: message, ObservedGeneration: p.Generation,
	})
}

// truncate keeps status messages small.
func truncate(s string) string {
	if len(s) > maxMessage {
		return s[len(s)-maxMessage:]
	}
	return s
}

// SetupWithManager registers the reconciler.
func (r *BackupPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&backupv1.BackupPolicy{}).Named("backuppolicy").Complete(r)
}
