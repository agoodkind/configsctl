locals {
  result = <<-TEXT
    ${upper(
      var.vault_demo
    )}
  TEXT
}
