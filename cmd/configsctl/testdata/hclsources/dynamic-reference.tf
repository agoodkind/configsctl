outer {
  dynamic "var" {
    for_each = [var.vault_demo]
    content { value = var.value }
  }
}
