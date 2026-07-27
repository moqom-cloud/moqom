output "instance_name" {
  value       = google_sql_database_instance.this.name
  description = "Cloud SQL instance name."
}

output "instance_connection_name" {
  value       = google_sql_database_instance.this.connection_name
  description = "project:region:instance, for the Cloud SQL connectors."
}

output "private_ip" {
  value       = google_sql_database_instance.this.private_ip_address
  description = "Private address the control plane dials. Reachable only from the peered VPC."
}

output "databases" {
  value       = [for db in google_sql_database.this : db.name]
  description = "Logical databases created."
}

output "iam_usernames" {
  value       = local.iam_usernames
  description = "Postgres role name for each IAM service account, keyed by email. Pass the value, not the email, as the connection username."
}

output "recording_bucket" {
  value       = google_storage_bucket.recordings.name
  description = "Managed recording bucket name."
}

output "recording_bucket_url" {
  value       = google_storage_bucket.recordings.url
  description = "gs:// URL of the recording bucket."
}

output "secret_ids" {
  value       = { for k, s in google_secret_manager_secret.this : k => s.secret_id }
  description = "Secret containers created. All are empty until someone adds a version."
}
