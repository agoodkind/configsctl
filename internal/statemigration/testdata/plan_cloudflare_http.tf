terraform {
  required_providers {
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "5.28.1"
    }
  }
}

variable "fixture_base_url" {
  type = string
}

provider "cloudflare" {
  base_url = var.fixture_base_url
}

data "cloudflare_dns_record" "diagnostic" {
  zone_id       = "00000000000000000000000000000000"
  dns_record_id = "11111111111111111111111111111111"
}
