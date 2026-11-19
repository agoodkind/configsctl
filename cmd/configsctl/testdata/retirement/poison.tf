terraform {
  required_version = "= 0.0.1"
}

resource "terraform_data" "unused" {
  input = file("this-file-must-never-be-read-by-the-verifier")
}
