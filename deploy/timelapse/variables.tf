variable "project" {
  type        = string
  description = "Project holding the bucket and the job"
}

variable "region" {
  type        = string
  description = "Region to run in. Put it where the bucket is: reading a month of masters across regions is the one cost this design is trying not to pay."
  default     = "us-west1"
}

variable "bucket" {
  type        = string
  description = "Bucket holding archive/ and receiving timelapse/. Not managed here."
}

variable "image" {
  type        = string
  description = "Pinned image. Pinned rather than :latest so a deploy is a decision, the same way the tower's is."
}

variable "timezone" {
  type        = string
  description = "Zone the archive's day directories are named in. Must match the daemon's TZ."
  default     = "America/Los_Angeles"
}

variable "schedule" {
  type        = string
  description = "When to build. After midnight, because a day is only worth encoding once it is over."
  default     = "30 0 * * *"
}

variable "settings" {
  type        = map(string)
  description = <<-EOT
    Extra environment for the job — TIMELAPSE_WIDTH, TIMELAPSE_CRF, TIMELAPSE_FPS,
    TIMELAPSE_MONTH_DAYS and so on. Changing any of the encoder settings changes
    the segment fingerprint, so the next run rebuilds the whole window under a
    new name rather than joining segments that no longer match.
  EOT
  default     = {}
}
