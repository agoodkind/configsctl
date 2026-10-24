resource "terraform_data" "other" {
  input = terraform_data.original.output
}
