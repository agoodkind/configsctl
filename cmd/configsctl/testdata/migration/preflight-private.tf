resource "terraform_data" "renamed" {
  input = var.vault_migration_fixture
  lifecycle {
    precondition {
      condition = (
        !fileexists("${path.module}/.env") &&
        !fileexists("${path.module}/.terraform/private-cache")
      )
      error_message = "The private plan copied an ignored runtime file."
    }
  }
}
