---
subcategory: "Domain Name Service (DNS)"
---

# huaweicloud\_dns\_ptrrecord

Manages a DNS PTR record in the HuaweiCloud DNS Service.
This is an alternative to `huaweicloud_dns_ptrrecord_v2`

## Example Usage

```hcl
resource "huaweicloud_vpc_eip" "eip_1" {
  publicip {
    type = "5_bgp"
  }
  bandwidth {
    name        = "test"
    size        = 5
    share_type  = "PER"
    charge_mode = "traffic"
  }
}

resource "huaweicloud_dns_ptrrecord" "ptr_1" {
  name          = "ptr.example.com."
  description   = "An example PTR record"
  floatingip_id = huaweicloud_vpc_eip.eip_1.id
  ttl           = 3000

  tags = {
    foo = "bar"
  }
}
```

## Argument Reference

The following arguments are supported:

* `region` - (Optional) The region in which to create the PTR record.
    If omitted, the `region` argument of the provider will be used.
    Changing this creates a new PTR record.

* `name` - (Required) Domain name of the PTR record. A domain name is case insensitive.
  Uppercase letters will also be converted into lowercase letters.

* `description` - (Optional) Description of the PTR record.

* `floatingip_id` - (Required) The ID of the FloatingIP/EIP.

* `ttl` - (Optional) The time to live (TTL) of the record set (in seconds). The value
  range is 300–2147483647. The default value is 300.

* `tags` - (Optional) Tags key/value pairs to associate with the PTR record.

## Attributes Reference

The following attributes are exported:

* `id` -  The PTR record ID, which is in {region}:{floatingip_id} format.
* `name` - See Argument Reference above.
* `description` - See Argument Reference above.
* `floatingip_id` - See Argument Reference above.
* `ttl` - See Argument Reference above.
* `tags` - See Argument Reference above.
* `address` - The address of the FloatingIP/EIP.

## Timeouts
This resource provides the following timeouts configuration options:
- `create` - Default is 10 minute.
- `update` - Default is 10 minute.
- `delete` - Default is 10 minute.

## Import

PTR records can be imported using region and floatingip/eip ID, separated by a colon(:), e.g.

```
$ terraform import huaweicloud_dns_ptrrecord.ptr_1 cn-north-1:d90ce693-5ccf-4136-a0ed-152ce412b6b9
```
