terraform {
  backend "local" { path = %q }
}

resource "terraform_data" "item" { input = "selected" }
resource "terraform_data" "other" { input = "other" }
