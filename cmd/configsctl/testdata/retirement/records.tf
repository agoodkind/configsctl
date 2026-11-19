resource "terraform_data" "older" {
  input = "private-record-value"
}
resource "terraform_data" "old" {
  count = terraform.workspace == "review" ? 1 : 0
  input = "private-record-value"
}
