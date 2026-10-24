variable "name" {
  type = string
}

variable "healthy" {
  type = bool
}

resource "terraform_data" "item" {
  input = "${var.name}-changed"

  lifecycle {
    precondition {
      condition = var.healthy
      error_message = "fixture precondition failed for ${var.name}"
    }
  }
}
