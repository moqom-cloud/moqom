# Change management

With one engineer there is no second human approver. In this program a change
counts as reviewed when it has passed the automated checks below and been
committed with a message that says why. The machines are the second reviewer.

## Path to production

| Step | What enforces it | Status |
| --- | --- | --- |
| 1. Change is a git commit (code, Terraform, or docs) | Habit; Terraform is the only way infrastructure is created | Implemented |
| 2. CI builds, vets, tests | `.github/workflows/ci.yml` (self-hosted runners for moqom and moqom-engine; the public SDK repos' hosted runners are blocked by the GitHub billing failure) | Partial |
| 3. Security gate passes | `deploy/tools/cloudbuild-security.yaml`: no HIGH/CRITICAL | **Planned (code ready)**: would fail today on 9 reachable Go vulnerabilities |
| 4. Image built by Cloud Build, pinned by digest in `terraform.tfvars` | `cloudbuild-control.yaml`, `cloudbuild-compliance.yaml` | Implemented |
| 5. `terraform plan` reviewed, then `apply` | Manual | Implemented (manual) |
| 6. Nightly check confirms the result and flags drift | `moqom-compliance` | Planned (code ready) |
| 7. Out-of-band changes alert | IAM, firewall and logging-tamper alerts | Planned (code ready) |

## Self-review checklist (for every non-trivial change)

Put it in the commit message or PR description:

- [ ] What changed and why
- [ ] Security effect: new exposure, new data, new permission?
- [ ] Personal data effect: new field, new retention, new processor?
- [ ] Rollback: how to undo it
- [ ] Docs: catalog or retention table updated if a control changed

## Emergency changes

Allowed during a SEV-1/2 without the gate. Within 2 business days: commit the
change to Terraform or code, run the gate, and note it in the incident review.

## Separation of environments

Only `deploy/envs/dev` exists, and testers use it. A `prod` root with its own
project, `REGIONAL` database, deletion protection and locked log and evidence
buckets is Planned (ISO 27001 A.8.31).

## Setting up the security gate trigger (Planned)

Once the GitHub repository is connected to Cloud Build (a console step, see
[README](README.md)):

```sh
gcloud builds triggers create github --project=moqom-503700 \
  --name=moqom-security --repo-owner=moqom-cloud --repo-name=moqom \
  --branch-pattern='^main$' \
  --build-config=deploy/tools/cloudbuild-security.yaml
```

A trigger builds from the moqom repository alone. The full multi-repository
scan needs the sibling checkouts, so until a wrapper exists, run it with
`gcloud builds submit` from the parent directory (exact command in the YAML
header).
