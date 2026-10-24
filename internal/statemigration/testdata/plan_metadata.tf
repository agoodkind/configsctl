resource "terraform_data" "additional" {
  input = {
    (var.fixture_value) = [var.fixture_value, "changed"]
  }
}
