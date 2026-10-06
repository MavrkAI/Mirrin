#!/bin/sh
# route53-dnssec.sh: sign the tenant zone, once (tenant-zone.md, step 3).
# Run by the maintainer with the AWS CLI and an admin profile, never by the
# control plane, whose IAM user can only write handle records.
#
# 1. Makes a KMS key for the key-signing key (ECC_NIST_P256, us-east-1,
#    as Route 53 requires), usable only by Route 53's DNSSEC service.
# 2. Creates the key-signing key and turns signing on.
# 3. Prints the DS record for the registrar.
#
# It stops at the first error and can be re-run: each step checks first.
set -eu
zone_id=@@ROUTE53_HOSTED_ZONE_ID@@
zone=@@TENANT_ZONE@@
ksk=mirrin_tenant_ksk_1
account=$(aws sts get-caller-identity --query Account --output text)

alias_name=alias/mirrin-tenant-dnssec
key_arn=$(aws kms describe-key --region us-east-1 --key-id "$alias_name" --query KeyMetadata.Arn --output text 2>/dev/null || true)
if [ -z "$key_arn" ] || [ "$key_arn" = None ]; then
	policy=$(cat <<POLICY
{
  "Version": "2012-10-17",
  "Statement": [
    {"Sid": "Admin", "Effect": "Allow", "Principal": {"AWS": "arn:aws:iam::$account:root"}, "Action": "kms:*", "Resource": "*"},
    {"Sid": "Route53DNSSEC", "Effect": "Allow", "Principal": {"Service": "dnssec-route53.amazonaws.com"},
     "Action": ["kms:DescribeKey", "kms:GetPublicKey", "kms:Sign"], "Resource": "*",
     "Condition": {"StringEquals": {"aws:SourceAccount": "$account"}, "ArnLike": {"aws:SourceArn": "arn:aws:route53:::hostedzone/*"}}},
    {"Sid": "Route53DNSSECGrant", "Effect": "Allow", "Principal": {"Service": "dnssec-route53.amazonaws.com"},
     "Action": "kms:CreateGrant", "Resource": "*", "Condition": {"Bool": {"kms:GrantIsForAWSResource": true}}}
  ]
}
POLICY
)
	key_arn=$(aws kms create-key --region us-east-1 --key-spec ECC_NIST_P256 --key-usage SIGN_VERIFY \
		--description "DNSSEC KSK for $zone" --policy "$policy" --query KeyMetadata.Arn --output text)
	aws kms create-alias --region us-east-1 --alias-name "$alias_name" --target-key-id "$key_arn"
	echo "made KMS key $key_arn"
fi

if ! aws route53 get-dnssec --hosted-zone-id "$zone_id" --query "KeySigningKeys[?Name=='$ksk'].Name" --output text | grep -q "$ksk"; then
	aws route53 create-key-signing-key --hosted-zone-id "$zone_id" --name "$ksk" \
		--key-management-service-arn "$key_arn" --status ACTIVE --caller-reference "$ksk-$(date +%s)"
fi
status=$(aws route53 get-dnssec --hosted-zone-id "$zone_id" --query Status.ServeSignature --output text)
if [ "$status" != SIGNING ]; then
	aws route53 enable-hosted-zone-dnssec --hosted-zone-id "$zone_id"
fi

echo
echo "The DS record for $zone, to add at the registrar:"
aws route53 get-dnssec --hosted-zone-id "$zone_id" --query "KeySigningKeys[?Name=='$ksk'].DSRecord" --output text
echo
echo "Then check, after the registrar publishes it:"
echo "  dig +dnssec $zone SOA @1.1.1.1   (the ad flag)"
echo "  https://dnsviz.net/d/$zone/dnssec/"
echo "Set a CloudWatch alarm on DNSSECInternalFailure and DNSSECKeySigningKeysNeedingAction for this zone (ALERTS.md)."
