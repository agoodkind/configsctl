moved {
  from = terraform_data.original
  to   = terraform_data.pending
}

resource "terraform_data" "pending" {
  input = var.vault_migration_fixture
}
