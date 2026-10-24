variable "fixture_order" {
  type    = list(string)
  default = ["first", "second"]
  validation {
    condition     = length(var.fixture_order) == 2
    error_message = "The synthetic ordered value requires two entries."
  }
}

variable "fixture_check" {
  type    = bool
  default = true
}

resource "terraform_data" "checked" {
  for_each = toset(["first", "second", "third"])
  input    = var.fixture_order
  lifecycle {
    precondition {
      condition     = length(var.fixture_value) > 0
      error_message = "The synthetic fixture value must exist."
    }
    postcondition {
      condition     = length(self.output) == 2
      error_message = "The synthetic ordered output requires two entries."
    }
  }
}

check "first" {
  assert {
    condition     = var.fixture_check && terraform_data.checked["first"].output[0] == var.fixture_order[0]
    error_message = "The first synthetic check failed."
  }
}

check "second" {
  assert {
    condition     = terraform_data.checked["second"].output[1] == var.fixture_order[1]
    error_message = "The second synthetic check failed."
  }
}
