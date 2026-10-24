variable "value" {
  type      = string
  sensitive = true
}

variable "number" {
  type = number
}

resource "terraform_data" "entry" {
  for_each = { old = true }
  input = {
    content   = var.value
    number    = var.number
    reference = "module.old[\"blue\"].terraform_data.entry[\"old\"]"
  }
}
