variable "message" {
  type    = string
  default = "default"
}

resource "terraform_data" "argument" {
  input = var.message
}

output "message" {
  value = terraform_data.argument.output
}
