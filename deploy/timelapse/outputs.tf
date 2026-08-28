output "job_name" {
  value       = google_cloud_run_v2_job.timelapse.name
  description = "Run it by hand with: gcloud run jobs execute <name> --region <region>"
}

output "service_account" {
  value       = google_service_account.timelapse.email
  description = "The identity holding objectAdmin on the bucket"
}
