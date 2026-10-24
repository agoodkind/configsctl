variable "value" {
  type      = string
  sensitive = true
}

variable "number" {
  type = number
}

resource "terraform_data" "renamed" {
  for_each = { new = true }
  input = {
    content   = var.value
    number    = var.number
    reference = "module.old[\"blue\"].terraform_data.entry[\"old\"]"
  }
}
