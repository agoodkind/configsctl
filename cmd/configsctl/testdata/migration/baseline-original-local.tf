resource "terraform_data" "original" {
  input = var.vault_migration_fixture
}

resource "local_file" "original_file" {
  filename             = CONFIGSCTL_LOCAL_FILE
  content              = var.vault_migration_fixture
  file_permission      = "0600"
  directory_permission = "0700"
}
