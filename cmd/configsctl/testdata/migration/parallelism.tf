terraform {
  required_providers {
    http = {
      source  = "hashicorp/http"
      version = "3.6.3"
    }
  }
}

variable "probe_endpoint" {
  type = string
}

data "http" "probe" {
  for_each = toset(["first", "second", "third"])
  url      = "${var.probe_endpoint}/MIGRATION_PLAN_STAGE/${each.value}"
}
