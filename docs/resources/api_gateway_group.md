---
subcategory: "API Gateway (APIG)"
---

# huaweicloud\_api\_gateway\_group

Provides an API gateway group resource.

## Example Usage

```hcl
resource "huaweicloud_api_gateway_group" "apigw_group" {
  name        = "apigw_group"
  description = "your descpiption"
}
```

## Argument Reference

The following arguments are supported:

* `region` - (Optional) The region in which to create the API gateway group resource. If omitted, the provider-level region will be used. Changing this creates a new gateway group resource.

* `name` - (Required) Specifies the name of the API group. An API group name consists of 3–64 characters,
    starting with a letter. Only letters, digits, and underscores (_) are allowed.

* `description` - (Optional) Specifies the description of the API group.
    The description cannot exceed 255 characters.

## Attributes Reference

The following attributes are exported:

* `id` - ID of the API group.
* `status` - Status of the API group.
* `name` - See Argument Reference above.
* `description` - See Argument Reference above.

## Timeouts
This resource provides the following timeouts configuration options:
- `create` - Default is 10 minute.
- `delete` - Default is 10 minute.

