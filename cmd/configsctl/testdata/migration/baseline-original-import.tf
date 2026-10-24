resource "terraform_data" "original" {
  input = var.vault_migration_fixture
}

resource "terraform_data" "unmapped_import" {}

import {
  to = terraform_data.unmapped_import
  id = "synthetic-private-import-identity-never-print"
}
