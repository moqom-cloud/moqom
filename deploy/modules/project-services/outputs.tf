output "services_ready" {
  value       = time_sleep.propagation.id
  description = "Opaque value to `depends_on` from anything that needs the APIs actually live."
}

output "enabled_services" {
  value       = [for s in google_project_service.this : s.service]
  description = "Services this module enabled."
}
