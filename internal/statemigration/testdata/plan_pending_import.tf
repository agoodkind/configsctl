resource "terraform_data" "pending" {}

import {
  to = terraform_data.pending
  id = "synthetic-private-import-identity-never-print"
}
