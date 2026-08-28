# The nightly timelapse job.
#
# It runs next to the bucket rather than on the tower. A month of archived
# masters is a couple of gigabytes to read, and the whole design exists to avoid
# spending the tower's uplink twice on the same frame — so the job reads the
# bucket from inside the same region, where that traffic costs nothing and
# crosses no radio link.
#
# The bucket itself is not managed here. It belongs to the deployment that
# publishes the frames, and this only asks for a role on it.
#
# This is the parameterised form, kept beside the code so the two do not drift.
# The deployed copy lives in the Rattlesnake Mountain infrastructure repository,
# where the project and bucket are concrete resources rather than variables.

terraform {
  required_version = ">= 1.5"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 5.0"
    }
  }
}

locals {
  job_name = "rattlecam-timelapse"
}

# The job's own identity. Separate from the tower's uploader: that one may only
# write, and this one has to read the whole archive back, which is a much larger
# capability and should not be attached to a key file living on a mountain.
resource "google_service_account" "timelapse" {
  project      = var.project
  account_id   = "rattlecam-timelapse"
  display_name = "rattlecam timelapse job"
  description  = "Reads archived frames and writes the day, week and month videos"
}

# objectUser rather than objectCreator, for the same reason the tower's uploader
# needs it: replacing an object requires delete as well as create, and
# latest-daily.mp4 is replaced every night. objectAdmin would also serve but adds
# setIamPolicy on objects, which nothing here uses. Granted on the bucket, not
# the project.
resource "google_storage_bucket_iam_member" "timelapse" {
  bucket = var.bucket
  role   = "roles/storage.objectUser"
  member = google_service_account.timelapse.member
}

resource "google_cloud_run_v2_job" "timelapse" {
  project  = var.project
  name     = local.job_name
  location = var.region

  deletion_protection = false

  template {
    task_count = 1

    template {
      service_account = google_service_account.timelapse.email

      # An hour. A steady night is a couple of minutes — one day's frames to
      # encode and a stream copy for each product — but the first run has no
      # segments at all and builds the whole window, which is thirty times the
      # work.
      timeout = "3600s"

      # Once. A failed night is not worth retrying immediately: whatever was
      # unreachable is unlikely to be reachable a second later, and tomorrow's
      # run rebuilds anything missing anyway, because a run that finds no segment
      # for a day builds it.
      max_retries = 1

      containers {
        # Pulled straight from ghcr.io. Cloud Run accepts public images from
        # there and caches them for an hour, which is irrelevant to something
        # that runs once a day. An Artifact Registry remote repository is the
        # more available arrangement and is what to reach for if the image is
        # ever made private.
        image = var.image

        resources {
          limits = {
            cpu = "4"
            # The work directory is a tmpfs on Cloud Run, so scratch space is
            # memory. The high-water mark is one day of frames (about 125 MB,
            # deleted as soon as they are encoded), the cached segments for the
            # window, and the largest product. Four gigabytes leaves room for
            # ffmpeg on top of a first-run backfill.
            memory = "4Gi"
          }
        }

        env {
          name  = "GCS_BUCKET"
          value = var.bucket
        }

        # The archive's day directories are named in this zone, because the
        # daemon writing them runs with this TZ set. A job resolving dates in
        # any other zone would build a day that starts in the afternoon.
        env {
          name  = "TZ"
          value = var.timezone
        }

        dynamic "env" {
          for_each = var.settings
          content {
            name  = env.key
            value = env.value
          }
        }
      }
    }
  }
}

# Cloud Scheduler calls the Admin API to start the job, so it needs an identity
# of its own and the right to invoke this one job.
resource "google_service_account" "scheduler" {
  project      = var.project
  account_id   = "rattlecam-timelapse-cron"
  display_name = "rattlecam timelapse scheduler"
}

resource "google_cloud_run_v2_job_iam_member" "invoke" {
  project  = var.project
  location = google_cloud_run_v2_job.timelapse.location
  name     = google_cloud_run_v2_job.timelapse.name
  role     = "roles/run.invoker"
  member   = google_service_account.scheduler.member
}

resource "google_cloud_scheduler_job" "nightly" {
  project  = var.project
  region   = var.region
  name     = "${local.job_name}-nightly"
  schedule = var.schedule

  # The schedule is read in the site's zone so it stays at half past midnight
  # through a daylight-saving change, rather than drifting an hour twice a year
  # and building a day that is not quite over.
  time_zone = var.timezone

  attempt_deadline = "320s" # starting the job, not running it

  retry_config {
    retry_count = 1
  }

  http_target {
    http_method = "POST"
    uri = join("", [
      "https://${var.region}-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/",
      var.project,
      "/jobs/",
      google_cloud_run_v2_job.timelapse.name,
      ":run",
    ])

    oauth_token {
      service_account_email = google_service_account.scheduler.email
    }
  }
}
