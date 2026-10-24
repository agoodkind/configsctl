terraform {
  required_providers {
    pveguest = {
      source = "tofu.home.arpa/agoodkind/pveguest"
      version = "0.1.0"
    }
  }
}

resource "pveguest_file" "item" {
  node = "site"
  vmid = 101
  kind = "lxc"
  path = "/tmp/configsctl-workspace-lock-fixture"
  content = "fixture"
}
