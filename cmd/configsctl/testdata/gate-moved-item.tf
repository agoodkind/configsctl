variable "name" {
  type = string
}

variable "healthy" {
  type = bool
}

resource "terraform_data" "renamed" {
  input = var.name

  lifecycle {
    precondition {
      condition     = var.healthy
      error_message = "fixture precondition failed for ${var.name}"
    }
  }
}

moved {
  from = terraform_data.item
  to   = terraform_data.renamed
}
