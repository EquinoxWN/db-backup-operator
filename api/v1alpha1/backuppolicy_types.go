package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types reported in status.conditions.
const (
	ConditionReady           = "Ready"
	ConditionBackupSucceeded = "BackupSucceeded"
	ConditionWALArchiving    = "WALArchiving"
)

// BackupPolicySpec says which PostgreSQL primary to back up and when.
//
// WAL-G itself is configured inside the database container (WALG_S3_PREFIX and storage
// credentials), where PostgreSQL's archive_command also needs it; the operator never sees
// storage credentials.
type BackupPolicySpec struct {
	// Selector matches the pod running the PostgreSQL primary; exactly one running pod must match.
	// +kubebuilder:validation:MinProperties=1
	Selector map[string]string `json:"selector"`

	// Container in that pod that has wal-g and psql installed.
	// +kubebuilder:default=postgres
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	Container string `json:"container,omitempty"`

	// PostgresUser is the database role psql uses to read archiver statistics.
	// +kubebuilder:default=postgres
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]{0,62}$`
	// +optional
	PostgresUser string `json:"postgresUser,omitempty"`

	// DataDir is PGDATA inside the container.
	// +kubebuilder:default=/var/lib/postgresql/data
	// +kubebuilder:validation:Pattern=`^/[A-Za-z0-9_./-]*$`
	// +optional
	DataDir string `json:"dataDir,omitempty"`

	// Schedule for base backups in standard five-field cron format, in UTC.
	// +kubebuilder:validation:MinLength=9
	Schedule string `json:"schedule"`

	// Suspend stops new backups without deleting the policy.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// BackupRecord is one attempted base backup.
type BackupRecord struct {
	// ScheduledTime is the cron time this run belongs to.
	ScheduledTime metav1.Time `json:"scheduledTime"`
	// StartTime and CompletionTime bracket the wal-g run.
	StartTime      metav1.Time `json:"startTime"`
	CompletionTime metav1.Time `json:"completionTime"`
	// Result is Succeeded or Failed.
	Result string `json:"result"`
	// Message holds the tail of the error output when the run failed.
	// +optional
	Message string `json:"message,omitempty"`
}

// WALArchiveStatus mirrors PostgreSQL's pg_stat_archiver view.
type WALArchiveStatus struct {
	ArchivedCount int64 `json:"archivedCount"`
	FailedCount   int64 `json:"failedCount"`
	// +optional
	LastArchivedWAL string `json:"lastArchivedWal,omitempty"`
	// +optional
	LastArchivedTime string `json:"lastArchivedTime,omitempty"`
	// +optional
	LastFailedTime string `json:"lastFailedTime,omitempty"`
}

// BackupPolicyStatus is what the controller observed.
type BackupPolicyStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
	// +optional
	LastSuccessfulBackupTime *metav1.Time `json:"lastSuccessfulBackupTime,omitempty"`
	// +optional
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`
	// +optional
	WALArchive *WALArchiveStatus `json:"walArchive,omitempty"`
	// History holds the most recent backup attempts, newest first.
	// +optional
	History []BackupRecord `json:"history,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// BackupPolicy schedules WAL-G base backups of one PostgreSQL primary and checks WAL archiving.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bp
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Last success",type=date,JSONPath=`.status.lastSuccessfulBackupTime`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type BackupPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupPolicySpec   `json:"spec"`
	Status BackupPolicyStatus `json:"status,omitempty"`
}

// BackupPolicyList is a list of BackupPolicy.
// +kubebuilder:object:root=true
type BackupPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupPolicy{}, &BackupPolicyList{})
}
