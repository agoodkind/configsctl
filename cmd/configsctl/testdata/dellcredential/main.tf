variable "poweredge_idrac_user" {
  type = string
  ephemeral = true
  sensitive = true
}
variable "poweredge_idrac_password" {
  type = string
  ephemeral = true
  sensitive = true
}
resource "terraform_data" "receipt" {
  input = "live-credential-lifecycle"
}
terraform {
  required_providers {
    restful = {
      source = "registry.terraform.io/magodo/restful"
      version = "0.25.2"
    }
  }
}
provider "restful" {
  base_url = "https://[3d06:bad:b01:300::efb]"
  client = { tls_insecure_skip_verify = true }
  security = { http = { basic = {
    username = var.poweredge_idrac_user
    password = "${var.poweredge_idrac_password}"
  } } }
}
data "restful_resource" "proof" {
  id = "/redfish/v1/Chassis/System.Embedded.1/NetworkAdapters/NIC.Embedded.1/NetworkDeviceFunctions/NIC.Embedded.1-1-1/Oem/Dell/DellNetworkAttributes/NIC.Embedded.1-1-1"
  method = "GET"
  output_attrs = ["Attributes.Broadcom_FlowOffload"]
}
output "active_flow_offload" {
  value = data.restful_resource.proof.output.Attributes.Broadcom_FlowOffload
}
