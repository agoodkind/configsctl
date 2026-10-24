moved {
  from = terraform_data.renamed
  to   = terraform_data.pending
}

resource "terraform_data" "pending" {
  input = var.vault_migration_fixture
}
