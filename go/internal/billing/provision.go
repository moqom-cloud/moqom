package billing

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Step is one idempotent piece of bringing a tenant up.
//
// The same shape as the heirloom Totum provisioner: steps run in order, each is
// recorded as it completes, and a failure undoes the completed ones in reverse.
// The GCP steps run only when an Infrastructure is configured, which is the seam
// the private tenancy SDK plugs into.
type Step string

const (
	StepRegisterTenant       Step = "register_tenant"
	StepCreateStripeCustomer Step = "create_stripe_customer"
	StepCreateServiceAccount Step = "create_service_account"
	StepCreateBucket         Step = "create_bucket"
	StepIssueAPIKey          Step = "issue_api_key"
)

type JobStatus string

const (
	JobRunning    JobStatus = "running"
	JobCompleted  JobStatus = "completed"
	JobFailed     JobStatus = "failed"
	JobRolledBack JobStatus = "rolled_back"
)

// Job is the record of one provisioning run.
type Job struct {
	ID          string
	Tenant      string
	Status      JobStatus
	Current     Step
	Completed   []Step
	Error       string
	CreatedAt   time.Time
	CompletedAt time.Time
}

// Infrastructure creates the Google Cloud resources a tenant gets of its own: a
// service account scoped to the tenant, and a bucket for its recordings. Every
// method must be idempotent, because a retried job calls it again.
type Infrastructure interface {
	CreateServiceAccount(ctx context.Context, tenant string) error
	DeleteServiceAccount(ctx context.Context, tenant string) error
	CreateBucket(ctx context.Context, tenant string) error
	DeleteBucket(ctx context.Context, tenant string) error
}

type step struct {
	name Step
	do   func(context.Context) error
	undo func(context.Context) error
}

// run executes steps, recording progress in the job, and rolls back on failure.
func (s *Service) run(ctx context.Context, job Job, steps []step) (Job, error) {
	job.Status = JobRunning
	if err := s.store.SaveJob(ctx, job); err != nil {
		return job, err
	}
	var done []step
	for _, next := range steps {
		job.Current = next.name
		if err := next.do(ctx); err != nil {
			job.Error = fmt.Sprintf("%s: %v", next.name, err)
			job.Status = JobFailed
			var undoErrors []error
			for i := len(done) - 1; i >= 0; i-- {
				if done[i].undo == nil {
					continue
				}
				if undoErr := done[i].undo(context.WithoutCancel(ctx)); undoErr != nil {
					undoErrors = append(undoErrors, fmt.Errorf("undo %s: %w", done[i].name, undoErr))
				}
			}
			if len(undoErrors) == 0 {
				job.Status = JobRolledBack
			} else {
				job.Error += "; " + errors.Join(undoErrors...).Error()
			}
			job.CompletedAt = s.now()
			_ = s.store.SaveJob(context.WithoutCancel(ctx), job)
			return job, fmt.Errorf("provisioning %s failed at %s: %w", job.Tenant, next.name, err)
		}
		done = append(done, next)
		job.Completed = append(job.Completed, next.name)
		if err := s.store.SaveJob(ctx, job); err != nil {
			return job, err
		}
	}
	job.Status = JobCompleted
	job.Current = ""
	job.CompletedAt = s.now()
	return job, s.store.SaveJob(ctx, job)
}
