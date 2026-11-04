data "terraform_remote_state" "fixture" {
  backend = "local"
  config = {
    path = "unread.tfstate"
  }
}
