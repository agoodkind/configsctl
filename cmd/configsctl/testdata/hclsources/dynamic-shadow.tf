outer {
  dynamic "item" {
    iterator = var
    for_each = []
    content { value = var.vault_demo }
  }
}
