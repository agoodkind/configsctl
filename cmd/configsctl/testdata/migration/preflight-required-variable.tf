variable "synthetic_required_diagnostic_value_never_print" {
  type      = string
  sensitive = true
}

resource "terraform_data" "renamed" {
  input = var.synthetic_required_diagnostic_value_never_print
}
