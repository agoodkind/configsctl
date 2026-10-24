resource "terraform_data" "unrelated" {
  count = 1
  input = "routing-fixture"
}
