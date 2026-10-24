terraform {
  backend "local" {
    path = CONFIGSCTL_MIGRATION_STATE_PATH
  }
}
