#!/usr/bin/env bash
#
# Deploy Agent Testnet to AWS using the AWS CLI.
#
# Usage:
#   Full demo (server + node + one client named "0"):
#     bash deploy/aws-deploy.sh deploy
#
#   Tier-by-tier deploy (each is idempotent and reuses shared network state):
#     bash deploy/aws-deploy.sh server-deploy
#     bash deploy/aws-deploy.sh node-deploy
#     bash deploy/aws-deploy.sh client-deploy [--client NAME]
#                                             [--server-url URL] [--join-token TOK]
#                                             [--instance-type TYPE]
#     bash deploy/aws-deploy.sh client-remove --client NAME
#
#   Operations (subcommands targeting clients support --client NAME; when only
#   one client exists it is auto-selected):
#     bash deploy/aws-deploy.sh status
#     bash deploy/aws-deploy.sh ssh <role> [--client NAME]
#     bash deploy/aws-deploy.sh restart <role> [--client NAME]
#     bash deploy/aws-deploy.sh redeploy <role> [--client NAME]
#     bash deploy/aws-deploy.sh logs <role> [--client NAME]
#     bash deploy/aws-deploy.sh reload                   # server-only
#     bash deploy/aws-deploy.sh test [--client NAME]
#     bash deploy/aws-deploy.sh openclaw <sub> [--client NAME] [...]
#     bash deploy/aws-deploy.sh teardown [--full]        # tears down all clients + server + node
#
# OpenClaw subcommands (each accepts --client NAME):
#   bash deploy/aws-deploy.sh openclaw install --api-key KEY [--provider anthropic|openai|xai|openrouter] [--openclaw-version 2026.5.7] [--persona NAME]
#   bash deploy/aws-deploy.sh openclaw chat      # Interactive chat with OpenClaw
#   bash deploy/aws-deploy.sh openclaw status    # Check OpenClaw + VM status
#   bash deploy/aws-deploy.sh openclaw stop      # Stop OpenClaw, proxies, and the VM
#   bash deploy/aws-deploy.sh openclaw reconfig --api-key KEY --provider openrouter --model anthropic/claude-haiku-4.5
#   bash deploy/aws-deploy.sh openclaw reconfig --client 1 --persona mrsmith --persona-confirm  # swap persona
#
# The --openclaw-version flag pins the npm version of openclaw installed in
# the agent VM. Daily OpenClaw releases regularly regress the agentic tool
# path, so install-openclaw.sh defaults to a tested version rather than the
# npm `latest` tag. See scripts/install-openclaw.sh header for details.
#
# The --persona flag uploads configs/personas/<NAME>/ to the agent VM and
# applies it (IDENTITY/SOUL/AGENTS/USER/HEARTBEAT.md + cron.json schedule).
# On `reconfig` it OVERWRITES those five workspace files and re-applies
# the persona's named cron jobs idempotently, so it requires
# --persona-confirm to acknowledge the overwrite (memory/, MEMORY.md, and
# anything else in the workspace are preserved). See configs/personas/README.md.
#
# Joining an existing testnet from a separate AWS account:
#   bash deploy/aws-deploy.sh client-deploy \
#     --server-url https://<server>:8443 --join-token <tok>
#
# Prerequisites:
#   - AWS CLI configured (aws sts get-caller-identity)
#   - Go 1.25+ (binaries are cross-compiled automatically if missing)
#   - deploy/install.sh present
#
# Resources are tagged with testnet-stack=agent-testnet for easy identification
# and cleanup. A state file (deploy/.aws-state.json) tracks all created
# resource IDs for teardown.
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
DIST_DIR="${PROJECT_DIR}/dist"

STACK_PREFIX="${STACK_PREFIX:-testnet}"
STACK_TAG="testnet-stack"
STACK_VALUE="${STACK_VALUE:-agent-testnet}"
STATE_FILE="${STATE_FILE:-${SCRIPT_DIR}/.aws-state.json}"
KEY_NAME="${KEY_NAME:-${STACK_PREFIX}-deploy-key}"
KEY_FILE="${KEY_FILE:-${SCRIPT_DIR}/.aws-${STACK_PREFIX}-key.pem}"

REGION="${AWS_REGION:-$(aws configure get region 2>/dev/null || echo "eu-west-1")}"

INSTANCE_TYPE_SERVER="${INSTANCE_TYPE_SERVER:-t3a.nano}"
INSTANCE_TYPE_NODE="${INSTANCE_TYPE_NODE:-t3a.nano}"
INSTANCE_TYPE_CLIENT="${INSTANCE_TYPE_CLIENT:-m8i-flex.large}"

NODES_YAML_SRC="${NODES_YAML_SRC:-${PROJECT_DIR}/configs/nodes.yaml}"
NODE_SECRET="$(head -c 16 /dev/urandom | base64 | tr -d '/+=' | head -c 24)"

# ---- shared helpers ----
#
# Logging, state file I/O, multi-client mgmt, SSH wrappers, ensure_binaries,
# nodes.yaml rendering, resolve_role_keys, mount_block_device, and the
# cloud-agnostic verbs (do_ssh / do_logs / do_reload / do_restart /
# do_redeploy / do_test / do_openclaw) all live in lib/common.sh and are
# shared with deploy/vultr-deploy.sh. Anything below is AWS-specific.

# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

tag_spec() {
    echo "ResourceType=$1,Tags=[{Key=${STACK_TAG},Value=${STACK_VALUE}},{Key=Name,Value=${STACK_PREFIX}-$2}]"
}

# ---- persistent resources (survive teardown) ----

ensure_eip() {
    local alloc_id
    alloc_id=$(load_state "eip_alloc_id")
    if [ -n "$alloc_id" ]; then
        if aws ec2 describe-addresses --region "$REGION" --allocation-ids "$alloc_id" >/dev/null 2>&1; then
            EIP_ALLOC="$alloc_id"
            EIP_PUBLIC=$(aws ec2 describe-addresses --region "$REGION" --allocation-ids "$alloc_id" \
                --query 'Addresses[0].PublicIp' --output text)
            info "Reusing Elastic IP: ${EIP_PUBLIC} (${EIP_ALLOC})"
            return
        fi
        warn "EIP ${alloc_id} from state no longer exists, allocating new one..."
    fi

    info "Allocating Elastic IP for server..."
    EIP_ALLOC=$(aws ec2 allocate-address --region "$REGION" --domain vpc \
        --tag-specifications "$(tag_spec elastic-ip server-eip)" \
        --query 'AllocationId' --output text)
    EIP_PUBLIC=$(aws ec2 describe-addresses --region "$REGION" --allocation-ids "$EIP_ALLOC" \
        --query 'Addresses[0].PublicIp' --output text)
    save_state "eip_alloc_id" "$EIP_ALLOC"
    save_state "eip_public_ip" "$EIP_PUBLIC"
    info "Allocated Elastic IP: ${EIP_PUBLIC} (${EIP_ALLOC})"
}

associate_eip() {
    local instance_id="$1"
    info "Associating Elastic IP ${EIP_PUBLIC} with server instance..."
    aws ec2 associate-address --region "$REGION" \
        --allocation-id "$EIP_ALLOC" --instance-id "$instance_id" >/dev/null
}

ensure_volume() {
    local role="$1" size="$2" az="$3"
    local vol_id
    vol_id=$(load_state "vol_${role}")
    if [ -n "$vol_id" ]; then
        if aws ec2 describe-volumes --region "$REGION" --volume-ids "$vol_id" >/dev/null 2>&1; then
            info "Reusing data volume for ${role}: ${vol_id}" >&2
            echo "$vol_id"
            return
        fi
        warn "Volume ${vol_id} for ${role} no longer exists, creating new one..." >&2
    fi

    info "Creating ${size} GiB data volume for ${role} in ${az}..." >&2
    vol_id=$(aws ec2 create-volume --region "$REGION" \
        --availability-zone "$az" \
        --size "$size" \
        --volume-type gp3 \
        --tag-specifications "$(tag_spec volume "${role}-data")" \
        --query 'VolumeId' --output text)
    save_state "vol_${role}" "$vol_id"
    info "Created volume ${vol_id} for ${role}" >&2
    echo "$vol_id"
}

attach_and_mount_volume() {
    local vol_id="$1" instance_id="$2" ip="$3" key="$4" mount_point="$5"

    info "Waiting for volume ${vol_id} to be available..."
    aws ec2 wait volume-available --region "$REGION" --volume-ids "$vol_id" 2>/dev/null || true

    info "Attaching ${vol_id} to ${instance_id}..."
    aws ec2 attach-volume --region "$REGION" \
        --volume-id "$vol_id" --instance-id "$instance_id" --device /dev/xvdf >/dev/null

    info "Waiting for volume to attach..."
    aws ec2 wait volume-in-use --region "$REGION" --volume-ids "$vol_id" 2>/dev/null || true
    sleep 3

    # Nitro instances expose the volume as /dev/nvme1n1 instead of /dev/xvdf;
    # mount_block_device tries each candidate in order.
    mount_block_device "$ip" "$key" "$mount_point" /dev/xvdf /dev/nvme1n1
}

detach_volume() {
    local role="$1"
    local vol_id
    vol_id=$(load_state "vol_${role}")
    [ -n "$vol_id" ] || return 0

    local vol_state
    vol_state=$(aws ec2 describe-volumes --region "$REGION" --volume-ids "$vol_id" \
        --query 'Volumes[0].State' --output text 2>/dev/null || echo "missing")

    if [ "$vol_state" = "in-use" ]; then
        info "Detaching data volume for ${role}: ${vol_id}..."
        aws ec2 detach-volume --region "$REGION" --volume-id "$vol_id" --force >/dev/null 2>&1 || true
        aws ec2 wait volume-available --region "$REGION" --volume-ids "$vol_id" 2>/dev/null || true
    fi
}

delete_volume() {
    local role="$1"
    local vol_id
    vol_id=$(load_state "vol_${role}")
    [ -n "$vol_id" ] || return 0

    info "Deleting data volume for ${role}: ${vol_id}..."
    aws ec2 delete-volume --region "$REGION" --volume-id "$vol_id" 2>/dev/null || true
}

# ---- preflight ----

preflight_check() {
    [ -f "${SCRIPT_DIR}/install.sh" ] || err "Missing deploy/install.sh"
    aws sts get-caller-identity >/dev/null 2>&1 || err "AWS CLI not configured"
}

# Get the public IP of an EC2 instance.
get_ip() {
    aws ec2 describe-instances --region "$REGION" --instance-ids "$1" \
        --query 'Reservations[0].Instances[0].PublicIpAddress' --output text
}

# ---- shared network resources (VPC / subnet / IGW / RTB / key pair / AMI) ----

# Resolve and cache the latest Ubuntu 24.04 amd64 AMI ID for this region.
# Sets $AMI_ID and persists it to state.
ensure_ami() {
    info "Finding Ubuntu 24.04 AMI in ${REGION}..."
    AMI_ID=$(aws ec2 describe-images \
        --region "$REGION" \
        --owners 099720109477 \
        --filters \
            "Name=name,Values=ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*" \
            "Name=state,Values=available" \
        --query 'Images | sort_by(@, &CreationDate) | [-1].ImageId' \
        --output text)
    [ "$AMI_ID" != "None" ] && [ -n "$AMI_ID" ] || err "Could not find Ubuntu 24.04 AMI"
    info "Using AMI: ${AMI_ID}"
    save_state "ami_id" "$AMI_ID"
}

# Create-or-load the VPC, IGW, subnet, route table, key pair, and AZ used
# by every tier. Idempotent — reuses anything already in state that still
# exists in AWS, otherwise creates fresh and saves.
#
# After this returns, the following globals are set:
#   AMI_ID, VPC_ID, IGW_ID, SUBNET_ID, RTB_ID, AZ, KEY_NAME, KEY_FILE
ensure_base_network() {
    ensure_ami

    # VPC
    VPC_ID=$(load_state "vpc_id")
    if [ -n "$VPC_ID" ] && \
       aws ec2 describe-vpcs --region "$REGION" --vpc-ids "$VPC_ID" >/dev/null 2>&1; then
        info "Reusing VPC: ${VPC_ID}"
    else
        # Warn about orphaned VPCs with our stack tag that aren't tracked.
        local orphan_vpcs
        orphan_vpcs=$(aws ec2 describe-vpcs --region "$REGION" \
            --filters "Name=tag:${STACK_TAG},Values=${STACK_VALUE}" \
            --query 'Vpcs[*].VpcId' --output text 2>/dev/null || true)
        if [ -n "$orphan_vpcs" ] && [ "$orphan_vpcs" != "None" ]; then
            warn "Found existing VPC(s) tagged ${STACK_TAG}=${STACK_VALUE}: ${orphan_vpcs}"
            warn "These may be orphaned from a previous deploy. Clean them up manually or"
            warn "use a different STACK_VALUE to avoid conflicts."
            warn "Continuing deploy with new resources..."
        fi
        info "Creating VPC..."
        VPC_ID=$(aws ec2 create-vpc \
            --region "$REGION" \
            --cidr-block "10.0.0.0/16" \
            --tag-specifications "$(tag_spec vpc vpc)" \
            --query 'Vpc.VpcId' --output text)
        save_state "vpc_id" "$VPC_ID"
        info "VPC: ${VPC_ID}"

        aws ec2 modify-vpc-attribute --region "$REGION" --vpc-id "$VPC_ID" --enable-dns-support '{"Value":true}'
        aws ec2 modify-vpc-attribute --region "$REGION" --vpc-id "$VPC_ID" --enable-dns-hostnames '{"Value":true}'
    fi

    # Internet Gateway
    IGW_ID=$(load_state "igw_id")
    if [ -n "$IGW_ID" ] && \
       aws ec2 describe-internet-gateways --region "$REGION" --internet-gateway-ids "$IGW_ID" >/dev/null 2>&1; then
        info "Reusing Internet Gateway: ${IGW_ID}"
    else
        info "Creating Internet Gateway..."
        IGW_ID=$(aws ec2 create-internet-gateway \
            --region "$REGION" \
            --tag-specifications "$(tag_spec internet-gateway igw)" \
            --query 'InternetGateway.InternetGatewayId' --output text)
        save_state "igw_id" "$IGW_ID"
        aws ec2 attach-internet-gateway --region "$REGION" --internet-gateway-id "$IGW_ID" --vpc-id "$VPC_ID"
    fi

    # Subnet (and AZ)
    SUBNET_ID=$(load_state "subnet_id")
    AZ=$(load_state "az")
    if [ -n "$SUBNET_ID" ] && \
       aws ec2 describe-subnets --region "$REGION" --subnet-ids "$SUBNET_ID" >/dev/null 2>&1; then
        info "Reusing subnet: ${SUBNET_ID} (${AZ})"
    else
        info "Creating subnet..."
        AZ=$(aws ec2 describe-availability-zones \
            --region "$REGION" \
            --query 'AvailabilityZones[0].ZoneName' --output text)
        SUBNET_ID=$(aws ec2 create-subnet \
            --region "$REGION" \
            --vpc-id "$VPC_ID" \
            --cidr-block "10.0.1.0/24" \
            --availability-zone "$AZ" \
            --tag-specifications "$(tag_spec subnet public)" \
            --query 'Subnet.SubnetId' --output text)
        save_state "subnet_id" "$SUBNET_ID"
        save_state "az" "$AZ"
        aws ec2 modify-subnet-attribute --region "$REGION" --subnet-id "$SUBNET_ID" --map-public-ip-on-launch
    fi

    # Route table
    RTB_ID=$(load_state "rtb_id")
    if [ -n "$RTB_ID" ] && \
       aws ec2 describe-route-tables --region "$REGION" --route-table-ids "$RTB_ID" >/dev/null 2>&1; then
        info "Reusing route table: ${RTB_ID}"
    else
        info "Configuring route table..."
        RTB_ID=$(aws ec2 create-route-table \
            --region "$REGION" \
            --vpc-id "$VPC_ID" \
            --tag-specifications "$(tag_spec route-table public)" \
            --query 'RouteTable.RouteTableId' --output text)
        save_state "rtb_id" "$RTB_ID"
        aws ec2 create-route --region "$REGION" --route-table-id "$RTB_ID" \
            --destination-cidr-block "0.0.0.0/0" --gateway-id "$IGW_ID" >/dev/null
        aws ec2 associate-route-table --region "$REGION" --route-table-id "$RTB_ID" --subnet-id "$SUBNET_ID" >/dev/null
    fi

    # SSH key pair
    if [ ! -f "$KEY_FILE" ]; then
        info "Creating SSH key pair..."
        aws ec2 delete-key-pair --region "$REGION" --key-name "$KEY_NAME" >/dev/null 2>&1 || true
        aws ec2 create-key-pair \
            --region "$REGION" \
            --key-name "$KEY_NAME" \
            --key-type ed25519 \
            --tag-specifications "$(tag_spec key-pair deploy-key)" \
            --query 'KeyMaterial' --output text > "$KEY_FILE"
        chmod 600 "$KEY_FILE"
    else
        info "Using existing SSH key: ${KEY_FILE}"
        aws ec2 describe-key-pairs --region "$REGION" --key-names "$KEY_NAME" >/dev/null 2>&1 || \
            err "Key file exists but key pair not in AWS. Delete ${KEY_FILE} and re-run."
    fi
    save_state "key_name" "$KEY_NAME"
    save_state "key_file" "$KEY_FILE"
}

# Create-or-load the server security group (API + WireGuard + DNS).
# Sets $SG_SERVER and persists it to state.
ensure_sg_server() {
    SG_SERVER=$(load_state "sg_server")
    if [ -n "$SG_SERVER" ] && \
       aws ec2 describe-security-groups --region "$REGION" --group-ids "$SG_SERVER" >/dev/null 2>&1; then
        info "Reusing server security group: ${SG_SERVER}"
        return
    fi

    info "Creating server security group..."
    SG_SERVER=$(aws ec2 create-security-group \
        --region "$REGION" \
        --group-name "${STACK_PREFIX}-server-sg" \
        --description "Testnet server: API + WireGuard + DNS" \
        --vpc-id "$VPC_ID" \
        --tag-specifications "$(tag_spec security-group server-sg)" \
        --query 'GroupId' --output text)
    save_state "sg_server" "$SG_SERVER"

    for rule in \
        "tcp 22 22 0.0.0.0/0" \
        "tcp 8443 8443 0.0.0.0/0" \
        "udp 51820 51820 0.0.0.0/0" \
        "udp 5353 5353 0.0.0.0/0" \
        "tcp 5353 5353 0.0.0.0/0"; do
        read -r proto from to cidr <<< "$rule"
        aws ec2 authorize-security-group-ingress --region "$REGION" \
            --group-id "$SG_SERVER" --protocol "$proto" \
            --port "${from}-${to}" --cidr "$cidr" >/dev/null
    done
}

# Create-or-load the node security group (HTTPS + SSH).
# Sets $SG_NODE and persists it to state.
ensure_sg_node() {
    SG_NODE=$(load_state "sg_node")
    if [ -n "$SG_NODE" ] && \
       aws ec2 describe-security-groups --region "$REGION" --group-ids "$SG_NODE" >/dev/null 2>&1; then
        info "Reusing node security group: ${SG_NODE}"
        return
    fi

    info "Creating node security group..."
    SG_NODE=$(aws ec2 create-security-group \
        --region "$REGION" \
        --group-name "${STACK_PREFIX}-node-sg" \
        --description "Testnet node: HTTPS" \
        --vpc-id "$VPC_ID" \
        --tag-specifications "$(tag_spec security-group node-sg)" \
        --query 'GroupId' --output text)
    save_state "sg_node" "$SG_NODE"

    for rule in \
        "tcp 22 22 0.0.0.0/0" \
        "tcp 443 443 0.0.0.0/0"; do
        read -r proto from to cidr <<< "$rule"
        aws ec2 authorize-security-group-ingress --region "$REGION" \
            --group-id "$SG_NODE" --protocol "$proto" \
            --port "${from}-${to}" --cidr "$cidr" >/dev/null
    done
}

# Create-or-load the client security group (SSH only).
# Sets $SG_CLIENT and persists it to state.
ensure_sg_client() {
    SG_CLIENT=$(load_state "sg_client")
    if [ -n "$SG_CLIENT" ] && \
       aws ec2 describe-security-groups --region "$REGION" --group-ids "$SG_CLIENT" >/dev/null 2>&1; then
        info "Reusing client security group: ${SG_CLIENT}"
        return
    fi

    info "Creating client security group..."
    SG_CLIENT=$(aws ec2 create-security-group \
        --region "$REGION" \
        --group-name "${STACK_PREFIX}-client-sg" \
        --description "Testnet client: SSH only" \
        --vpc-id "$VPC_ID" \
        --tag-specifications "$(tag_spec security-group client-sg)" \
        --query 'GroupId' --output text)
    save_state "sg_client" "$SG_CLIENT"

    aws ec2 authorize-security-group-ingress --region "$REGION" \
        --group-id "$SG_CLIENT" --protocol tcp \
        --port "22-22" --cidr "0.0.0.0/0" >/dev/null
}

# Launch a new EC2 instance and persist its id under instance_<state_key>.
# Args:
#   $1 state_key   e.g. "server", "node", "client_0", "client_alice"
#   $2 name_tag    suffix for the Name tag (e.g. "server", "client-alice")
#   $3 sg          security group id
#   $4 itype       instance type
#   $5 cpu_opts    optional --cpu-options string
# Echoes the instance id on stdout.
launch_instance() {
    local state_key="$1" name_tag="$2" sg="$3" itype="$4" cpu_opts="${5:-}"
    info "Launching ${name_tag} instance (${itype})..." >&2
    local cpu_flag=()
    if [ -n "$cpu_opts" ]; then
        cpu_flag=(--cpu-options "$cpu_opts")
    fi
    local instance_id
    instance_id=$(aws ec2 run-instances \
        --region "$REGION" \
        --image-id "$AMI_ID" \
        --instance-type "$itype" \
        --key-name "$KEY_NAME" \
        --subnet-id "$SUBNET_ID" \
        --security-group-ids "$sg" \
        --block-device-mappings '[{"DeviceName":"/dev/sda1","Ebs":{"VolumeSize":20,"VolumeType":"gp3"}}]' \
        --tag-specifications "$(tag_spec instance "$name_tag")" \
        "${cpu_flag[@]}" \
        --query 'Instances[0].InstanceId' --output text)
    save_state "instance_${state_key}" "$instance_id"
    echo "$instance_id"
}

# ---- role deploys ----

# Deploy the server tier: ensure network resources, launch the server EC2,
# attach the EIP and data volume, install testnet-server, retrieve the join
# token. Idempotent on already-running deployments — refuses to clobber.
do_server_deploy() {
    info "Deploying server to AWS (${REGION})"
    preflight_check
    require_nodes_yaml

    local existing
    existing=$(load_state "instance_server")
    if [ -n "$existing" ] && \
       [ "$(aws ec2 describe-instances --region "$REGION" --instance-ids "$existing" \
            --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null)" = "running" ]; then
        err "Server already deployed (instance ${existing}). Run 'restart server' or 'redeploy server'."
    fi

    ensure_binaries testnet-server
    ensure_base_network
    ensure_sg_server
    ensure_eip

    local NODE_NAME
    NODE_NAME=$(extract_node_name)
    save_state "node_name" "$NODE_NAME"
    info "Node name from config: ${NODE_NAME}"

    # Reuse a previously generated node secret so it stays stable across
    # server/node redeploys (the node side authenticates with this value).
    local NODE_SECRET_VAL
    NODE_SECRET_VAL=$(load_state "node_secret")
    if [ -z "$NODE_SECRET_VAL" ]; then
        NODE_SECRET_VAL="$NODE_SECRET"
        save_state "node_secret" "$NODE_SECRET_VAL"
    fi

    local VOL_SERVER
    VOL_SERVER=$(ensure_volume "server" 5 "$AZ")

    local INST_SERVER IP_SERVER
    INST_SERVER=$(launch_instance "server" "server" "$SG_SERVER" "$INSTANCE_TYPE_SERVER")
    info "Waiting for server instance to be running..."
    aws ec2 wait instance-running --region "$REGION" --instance-ids "$INST_SERVER"

    associate_eip "$INST_SERVER"
    IP_SERVER="$EIP_PUBLIC"
    save_state "ip_server" "$IP_SERVER"
    save_state "server_url" "https://${IP_SERVER}:8443"
    info "Server IP: ${IP_SERVER}"

    wait_for_ssh "$IP_SERVER" "$KEY_FILE"
    attach_and_mount_volume "$VOL_SERVER" "$INST_SERVER" "$IP_SERVER" "$KEY_FILE" "/opt/testnet/data"

    # Render nodes.yaml: at this point IP_NODE may be unknown (server-only
    # deploy) — render with whatever's in state, falling back to the
    # placeholder. The do_node_deploy reload step will overwrite it later.
    local IP_NODE_KNOWN
    IP_NODE_KNOWN=$(load_state "ip_node")
    [ -z "$IP_NODE_KNOWN" ] && IP_NODE_KNOWN="DEPLOY_NODE_ADDRESS"
    local nodes_tmp="${SCRIPT_DIR}/.nodes-deploy.yaml"
    render_nodes_yaml "$IP_NODE_KNOWN" "$NODE_SECRET_VAL" "$nodes_tmp"

    info "Installing server software..."
    remote_copy "$KEY_FILE" "${DIST_DIR}/testnet-server-linux-amd64" "ubuntu@${IP_SERVER}:/tmp/testnet-server"
    remote_copy "$KEY_FILE" "${SCRIPT_DIR}/install.sh" "ubuntu@${IP_SERVER}:/tmp/install.sh"
    remote_copy "$KEY_FILE" "$nodes_tmp" "ubuntu@${IP_SERVER}:/tmp/nodes.yaml"
    rm -f "$nodes_tmp"

    remote_exec "$IP_SERVER" "$KEY_FILE" "
        sudo mkdir -p /usr/local/bin
        sudo mv /tmp/testnet-server /usr/local/bin/testnet-server
        sudo chmod +x /usr/local/bin/testnet-server
        export AUTO_START=1
        sudo -E bash /tmp/install.sh server
    "

    info "Retrieving join token from server..."
    local JOIN_TOKEN=""
    for _ in $(seq 1 10); do
        JOIN_TOKEN=$(remote_exec "$IP_SERVER" "$KEY_FILE" "sudo cat /opt/testnet/data/join-token 2>/dev/null" || echo "")
        [ -n "$JOIN_TOKEN" ] && break
        sleep 3
    done
    if [ -z "$JOIN_TOKEN" ]; then
        err "Could not retrieve join token after 30s. Check: ssh -i ${KEY_FILE} ubuntu@${IP_SERVER} 'sudo journalctl -u testnet-server -f'"
    fi
    info "Join token: ${JOIN_TOKEN}"
    save_state "join_token" "$JOIN_TOKEN"

    if remote_exec "$IP_SERVER" "$KEY_FILE" "sudo systemctl is-active --quiet testnet-server" 2>/dev/null; then
        info "Server: healthy"
    else
        warn "Server: testnet-server service not active. Check: bash deploy/aws-deploy.sh logs server"
    fi
}

# Deploy the node tier: launch a node EC2, install testnet-node, then push
# the updated nodes.yaml (now including this node's address) to the server.
do_node_deploy() {
    info "Deploying node to AWS (${REGION})"
    preflight_check
    require_nodes_yaml

    local existing
    existing=$(load_state "instance_node")
    if [ -n "$existing" ] && \
       [ "$(aws ec2 describe-instances --region "$REGION" --instance-ids "$existing" \
            --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null)" = "running" ]; then
        err "Node already deployed (instance ${existing}). Run 'restart node' or 'redeploy node'."
    fi

    local server_url node_secret
    server_url=$(load_state "server_url")
    node_secret=$(load_state "node_secret")
    [ -n "$server_url" ]   || err "No server_url in state. Deploy the server first or set it manually."
    [ -n "$node_secret" ]  || err "No node_secret in state. Deploy the server first."

    ensure_binaries testnet-node testnet-toolkit
    ensure_base_network
    ensure_sg_node

    local NODE_NAME
    NODE_NAME=$(extract_node_name)
    save_state "node_name" "$NODE_NAME"

    local VOL_NODE
    VOL_NODE=$(ensure_volume "node" 10 "$AZ")

    local INST_NODE IP_NODE
    INST_NODE=$(launch_instance "node" "node" "$SG_NODE" "$INSTANCE_TYPE_NODE")
    info "Waiting for node instance to be running..."
    aws ec2 wait instance-running --region "$REGION" --instance-ids "$INST_NODE"

    IP_NODE=$(get_ip "$INST_NODE")
    save_state "ip_node" "$IP_NODE"
    info "Node IP: ${IP_NODE}"

    wait_for_ssh "$IP_NODE" "$KEY_FILE"
    attach_and_mount_volume "$VOL_NODE" "$INST_NODE" "$IP_NODE" "$KEY_FILE" "/opt/testnet"

    info "Installing node software..."
    remote_copy "$KEY_FILE" "${DIST_DIR}/testnet-node-linux-amd64" "ubuntu@${IP_NODE}:/tmp/testnet-node"
    remote_copy "$KEY_FILE" "${DIST_DIR}/testnet-toolkit-linux-amd64" "ubuntu@${IP_NODE}:/tmp/testnet-toolkit"
    remote_copy "$KEY_FILE" "${SCRIPT_DIR}/install.sh" "ubuntu@${IP_NODE}:/tmp/install.sh"

    remote_exec "$IP_NODE" "$KEY_FILE" "
        sudo mkdir -p /usr/local/bin
        sudo mv /tmp/testnet-node /usr/local/bin/testnet-node
        sudo chmod +x /usr/local/bin/testnet-node
        sudo mv /tmp/testnet-toolkit /usr/local/bin/testnet-toolkit
        sudo chmod +x /usr/local/bin/testnet-toolkit
        export SERVER_URL='${server_url}'
        export NODE_NAME='${NODE_NAME}'
        export NODE_SECRET='${node_secret}'
        sudo -E bash /tmp/install.sh node
    "

    # Push updated nodes.yaml (with this node's real address) to the server
    # and reload it so the new node entry is routable.
    local ip_server
    ip_server=$(load_state "ip_server")
    if [ -n "$ip_server" ]; then
        info "Updating server nodes.yaml with node address..."
        local nodes_tmp="${SCRIPT_DIR}/.nodes-deploy.yaml"
        render_nodes_yaml "$IP_NODE" "$node_secret" "$nodes_tmp"
        remote_copy "$KEY_FILE" "$nodes_tmp" "ubuntu@${ip_server}:/tmp/nodes.yaml"
        rm -f "$nodes_tmp"
        remote_exec "$ip_server" "$KEY_FILE" "
            sudo cp /tmp/nodes.yaml /opt/testnet/configs/nodes.yaml
            sudo bash -c 'rm -rf /opt/testnet/data/certs/*'
            sudo systemctl reload testnet-server
        "
    else
        warn "Server not deployed locally — skip nodes.yaml reload. Apply on the remote server manually."
    fi

    local node_ok=false
    for _ in $(seq 1 12); do
        if remote_exec "$IP_NODE" "$KEY_FILE" "sudo systemctl is-active --quiet testnet-node" 2>/dev/null; then
            node_ok=true
            break
        fi
        sleep 5
    done
    if $node_ok; then
        info "Node: healthy"
    else
        warn "Node: testnet-node service not active. Check: bash deploy/aws-deploy.sh logs node"
    fi
}

# Deploy a client tier instance under the supplied name (default: next free
# numeric index). Each named client gets its own EC2, SSH key reuse, and
# data volume keyed under instance_client_<name> / ip_client_<name> /
# vol_client_<name>. Many clients can be registered in the same state file.
#
# Args:
#   $1 name           client name (empty = auto-assign next index)
#   $2 server_url     optional override of state's server_url
#   $3 join_token     optional override of state's join_token
#   $4 instance_type  optional override of $INSTANCE_TYPE_CLIENT
do_client_deploy() {
    local name="${1:-}" override_server_url="${2:-}" override_join_token="${3:-}" override_itype="${4:-}"

    info "Deploying client to AWS (${REGION})"
    preflight_check

    [ -z "$name" ] && name=$(next_client_index)

    if is_registered_client "$name"; then
        local existing
        existing=$(load_state "instance_client_${name}")
        if [ -n "$existing" ] && \
           [ "$(aws ec2 describe-instances --region "$REGION" --instance-ids "$existing" \
                --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null)" = "running" ]; then
            err "Client '${name}' already deployed (instance ${existing}). Pick a different --client name or 'client-remove' first."
        fi
    fi

    # Resolve server URL + join token: command-line flags win, then state.
    local server_url join_token
    if [ -n "$override_server_url" ]; then
        server_url="$override_server_url"
        save_state "server_url" "$server_url"
    else
        server_url=$(load_state "server_url")
    fi
    if [ -n "$override_join_token" ]; then
        join_token="$override_join_token"
        save_state "join_token" "$join_token"
    else
        join_token=$(load_state "join_token")
    fi
    [ -n "$server_url" ] || err "No server_url. Pass --server-url URL or deploy a server first."
    [ -n "$join_token" ] || err "No join_token. Pass --join-token TOK or deploy a server first."

    local itype="${override_itype:-$INSTANCE_TYPE_CLIENT}"

    ensure_binaries testnet-client
    ensure_base_network
    ensure_sg_client

    local VOL_CLIENT
    VOL_CLIENT=$(ensure_volume "client_${name}" 20 "$AZ")

    local INST_CLIENT IP_CLIENT
    INST_CLIENT=$(launch_instance "client_${name}" "client-${name}" "$SG_CLIENT" "$itype" "NestedVirtualization=enabled")
    info "Waiting for client instance to be running..."
    aws ec2 wait instance-running --region "$REGION" --instance-ids "$INST_CLIENT"

    IP_CLIENT=$(get_ip "$INST_CLIENT")
    save_state "ip_client_${name}" "$IP_CLIENT"
    info "Client '${name}' IP: ${IP_CLIENT}"

    wait_for_ssh "$IP_CLIENT" "$KEY_FILE"
    attach_and_mount_volume "$VOL_CLIENT" "$INST_CLIENT" "$IP_CLIENT" "$KEY_FILE" "/root/.testnet"

    info "Installing client software..."
    remote_copy "$KEY_FILE" "${DIST_DIR}/testnet-client-linux-amd64" "ubuntu@${IP_CLIENT}:/tmp/testnet-client"
    remote_copy "$KEY_FILE" "${SCRIPT_DIR}/install.sh" "ubuntu@${IP_CLIENT}:/tmp/install.sh"

    remote_exec "$IP_CLIENT" "$KEY_FILE" "
        sudo mkdir -p /usr/local/bin
        sudo mv /tmp/testnet-client /usr/local/bin/testnet-client
        sudo chmod +x /usr/local/bin/testnet-client
        export SERVER_URL='${server_url}'
        export JOIN_TOKEN='${join_token}'
        sudo -E bash /tmp/install.sh client
    "

    register_client "$name"

    local wg_status
    wg_status=$(remote_exec "$IP_CLIENT" "$KEY_FILE" "sudo wg show wg-testnet 2>/dev/null | grep 'latest handshake'" 2>/dev/null || echo "")
    if [ -n "$wg_status" ]; then
        info "Client '${name}': healthy (WireGuard tunnel established)"
    else
        warn "Client '${name}': WireGuard tunnel not established yet"
    fi

    if remote_exec "$IP_CLIENT" "$KEY_FILE" "test -e /dev/kvm" 2>/dev/null; then
        info "Client '${name}': /dev/kvm available (Firecracker ready)"
    else
        warn "Client '${name}': /dev/kvm not found (Firecracker VMs will not work)"
    fi

    echo ""
    echo "  Client '${name}' deployed:"
    echo "    IP:  ${IP_CLIENT}"
    echo "    SSH: ssh -i ${KEY_FILE} ubuntu@${IP_CLIENT}"
    echo ""
    echo "  Run OpenClaw on this client:"
    echo "    bash deploy/aws-deploy.sh openclaw install --client ${name} --api-key ..."
    echo ""
}

# Tear down a single client (terminate its EC2, detach its volume) without
# touching shared infra or other clients. The volume is preserved by default.
do_client_remove() {
    local name="${1:-}"
    [ -n "$name" ] || err "Usage: client-remove --client NAME"
    is_registered_client "$name" || err "Unknown client '${name}'. Known: $(client_names_list)"

    local inst_id
    inst_id=$(load_state "instance_client_${name}")
    if [ -n "$inst_id" ]; then
        info "Terminating client '${name}' (${inst_id})..."
        # Detach volume first so it survives termination.
        detach_volume "client_${name}"
        aws ec2 terminate-instances --region "$REGION" --instance-ids "$inst_id" >/dev/null 2>&1 || true
        aws ec2 wait instance-terminated --region "$REGION" --instance-ids "$inst_id" 2>/dev/null || true
    fi

    delete_state "instance_client_${name}"
    delete_state "ip_client_${name}"
    unregister_client "$name"

    info "Client '${name}' removed (volume preserved as vol_client_${name})."
}

# Full deploy: server + node + a single client named "0".
do_deploy() {
    info "Deploying full Agent Testnet stack to AWS (${REGION})"

    preflight_check
    require_nodes_yaml

    if [ -f "$STATE_FILE" ]; then
        local existing_vpc
        existing_vpc=$(load_state "vpc_id")
        local existing_server
        existing_server=$(load_state "instance_server")
        if [ -n "$existing_vpc" ] && [ -n "$existing_server" ] && \
           aws ec2 describe-instances --region "$REGION" --instance-ids "$existing_server" \
               --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null \
           | grep -q running; then
            err "Active deployment found (VPC: ${existing_vpc}, server: ${existing_server}). Run 'teardown' first or use server-deploy/node-deploy/client-deploy for incremental updates."
        fi
    fi

    # Build everything up-front so all role deploys reuse the binaries.
    ensure_binaries testnet-server testnet-client testnet-node testnet-toolkit

    do_server_deploy
    do_node_deploy
    do_client_deploy "0"

    # ---- Summary ----
    local IP_SERVER IP_NODE IP_CLIENT0 NODE_NAME JOIN_TOKEN VPC_ID
    IP_SERVER=$(load_state "ip_server")
    IP_NODE=$(load_state "ip_node")
    IP_CLIENT0=$(load_state "ip_client_0")
    NODE_NAME=$(load_state "node_name")
    JOIN_TOKEN=$(load_state "join_token")
    VPC_ID=$(load_state "vpc_id")

    echo ""
    echo "============================================"
    echo "  Agent Testnet deployed on AWS"
    echo "============================================"
    echo ""
    echo "  Region:  ${REGION}"
    echo "  VPC:     ${VPC_ID}"
    echo ""
    echo "  Server:    ${IP_SERVER} (EIP)"
    echo "  Node:      ${IP_NODE}"
    echo "  Client 0:  ${IP_CLIENT0}"
    echo ""
    echo "  SSH key:    ${KEY_FILE}"
    echo "  Node name:  ${NODE_NAME}"
    echo "  Join token: ${JOIN_TOKEN}"
    echo ""
    echo "  Add another client (in this AWS account):"
    echo "    bash deploy/aws-deploy.sh client-deploy --client alice"
    echo ""
    echo "  Join from a different AWS account:"
    echo "    bash deploy/aws-deploy.sh client-deploy --server-url https://${IP_SERVER}:8443 --join-token ${JOIN_TOKEN}"
    echo ""
    echo "  Teardown (preserves EIP + data volumes):"
    echo "    bash deploy/aws-deploy.sh teardown"
    echo "  Full teardown (destroys everything):"
    echo "    bash deploy/aws-deploy.sh teardown --full"
    echo ""
    echo "  Instance types: server=${INSTANCE_TYPE_SERVER}, node=${INSTANCE_TYPE_NODE}, client=${INSTANCE_TYPE_CLIENT}"
    echo ""
}

# ---- status ----

do_status() {
    if [ ! -f "$STATE_FILE" ]; then
        err "No state file found. Run 'deploy' first."
    fi

    local key_file ip_server
    key_file=$(load_state "key_file")
    ip_server=$(load_state "ip_server")

    echo ""
    echo "Agent Testnet Status (${REGION})"
    echo "================================"

    # Show persistent resources even when no instances are running
    local eip_alloc eip_ip
    eip_alloc=$(load_state "eip_alloc_id")
    eip_ip=$(load_state "eip_public_ip")
    if [ -n "$eip_alloc" ]; then
        echo ""
        echo "Elastic IP:  ${eip_ip} (${eip_alloc})"
    fi
    for role in server node; do
        local vid
        vid=$(load_state "vol_${role}")
        [ -n "$vid" ] && echo "Volume (${role}): ${vid}"
    done
    local cnames
    cnames=$(client_names_list)
    for cname in $cnames; do
        local vid
        vid=$(load_state "vol_client_${cname}")
        [ -n "$vid" ] && echo "Volume (client/${cname}): ${vid}"
    done

    local inst_server
    inst_server=$(load_state "instance_server")
    if [ -z "$inst_server" ] && [ -z "$cnames" ]; then
        local inst_node
        inst_node=$(load_state "instance_node")
        if [ -z "$inst_node" ]; then
            echo ""
            echo "No active instances. Run 'deploy' or 'client-deploy' to launch."
            echo ""
            return
        fi
    fi
    echo ""

    # -- EC2 instances --
    echo "Instances:"
    printf "  %-12s  %-20s  %-16s  %-10s  %s\n" "ROLE" "INSTANCE" "IP" "EC2" "SERVICE"
    printf "  %-12s  %-20s  %-16s  %-10s  %s\n" "------------" "--------------------" "----------------" "----------" "-------"

    print_status_row() {
        local label="$1" inst_id="$2" ip="$3" svc_name="$4"
        local ec2_state svc_state
        ec2_state=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$inst_id" \
            --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null || echo "unknown")
        if [ "$ec2_state" = "running" ] && [ -f "$key_file" ]; then
            svc_state=$(remote_exec "$ip" "$key_file" \
                "systemctl is-active $svc_name 2>/dev/null || true" 2>/dev/null) || svc_state="ssh-err"
        else
            svc_state="-"
        fi
        printf "  %-12s  %-20s  %-16s  %-10s  %s\n" "$label" "$inst_id" "$ip" "$ec2_state" "$svc_state"
    }

    for role in server node; do
        local inst_id ip
        inst_id=$(load_state "instance_${role}")
        ip=$(load_state "ip_${role}")
        [ -z "$inst_id" ] && continue
        print_status_row "$role" "$inst_id" "$ip" "testnet-${role}"
    done
    for cname in $cnames; do
        local inst_id ip
        inst_id=$(load_state "instance_client_${cname}")
        ip=$(load_state "ip_client_${cname}")
        [ -z "$inst_id" ] && continue
        print_status_row "client/${cname}" "$inst_id" "$ip" "testnet-client"
    done

    # -- Registered nodes & domains (from server) --
    if [ -n "$ip_server" ] && [ -f "$key_file" ]; then
        local nodes_yaml
        nodes_yaml=$(remote_exec "$ip_server" "$key_file" \
            "sudo cat /opt/testnet/configs/nodes.yaml 2>/dev/null" 2>/dev/null) || true

        if [ -n "$nodes_yaml" ]; then
            echo ""
            echo "Registered nodes (from server nodes.yaml):"
            printf "  %-12s  %-24s  %s\n" "NAME" "ADDRESS" "DOMAINS"
            printf "  %-12s  %-24s  %s\n" "------------" "------------------------" "-------"
            echo "$nodes_yaml" | awk '
                /- name:/    { if (name) printf "  %-12s  %-24s  %s\n", name, addr, domains;
                               gsub(/.*name: *"?/, ""); gsub(/".*/, ""); name=$0; addr=""; domains="" }
                /address:/   { gsub(/.*address: *"?/, ""); gsub(/".*/, ""); addr=$0 }
                /^ *- "/ || /^ *- '\''/ { gsub(/.*- *"?/, ""); gsub(/".*/, ""); gsub(/'\''.*/, "");
                               if (domains) domains = domains ", " $0; else domains=$0 }
                END          { if (name) printf "  %-12s  %-24s  %s\n", name, addr, domains }
            '
        fi

        # -- WireGuard peers (connected clients) --
        echo ""
        echo "WireGuard peers:"
        remote_exec "$ip_server" "$key_file" \
            "sudo wg show wg0 2>/dev/null | grep -E '(peer|endpoint|latest handshake|transfer)' | sed 's/^/  /'" 2>/dev/null || echo "  (no tunnel or wg not running)"

        # -- Recent server log --
        echo ""
        echo "Server log (last 5 lines):"
        remote_exec "$ip_server" "$key_file" \
            "sudo journalctl -u testnet-server --no-pager -n 5 --output short-iso 2>/dev/null | sed 's/^/  /'" 2>/dev/null || echo "  (unavailable)"
    fi

    if [ -n "$ip_server" ]; then
        echo ""
        echo "Server URL: https://${ip_server}:8443"
    fi

    local join_token
    join_token=$(load_state "join_token")
    if [ -n "$join_token" ]; then
        echo ""
        echo "Join token: ${join_token}"
    fi

    echo ""
    echo "SSH: ssh -i ${key_file} ubuntu@<IP>"
    echo ""
}

# ---- teardown ----

do_teardown() {
    local full_teardown=false
    if [ "${1:-}" = "--full" ]; then
        full_teardown=true
        info "Full teardown requested -- EIP and data volumes will be destroyed."
    fi

    if [ ! -f "$STATE_FILE" ]; then
        err "No state file found. Nothing to tear down."
    fi

    info "Tearing down Agent Testnet in ${REGION}..."
    local teardown_errors=0

    local cnames
    cnames=$(client_names_list)

    # Detach persistent data volumes before terminating instances
    for role in server node; do
        detach_volume "$role"
    done
    for cname in $cnames; do
        detach_volume "client_${cname}"
    done

    # Terminate instances (including any extra tagged instances in this VPC)
    local vpc_id
    vpc_id=$(load_state "vpc_id")

    local all_instances=""
    for role in server node; do
        local inst_id
        inst_id=$(load_state "instance_${role}")
        if [ -n "$inst_id" ]; then
            info "Terminating ${role} instance: ${inst_id}..."
            aws ec2 terminate-instances --region "$REGION" --instance-ids "$inst_id" >/dev/null 2>&1 || true
            all_instances="${all_instances} ${inst_id}"
        fi
    done
    for cname in $cnames; do
        local inst_id
        inst_id=$(load_state "instance_client_${cname}")
        if [ -n "$inst_id" ]; then
            info "Terminating client/${cname} instance: ${inst_id}..."
            aws ec2 terminate-instances --region "$REGION" --instance-ids "$inst_id" >/dev/null 2>&1 || true
            all_instances="${all_instances} ${inst_id}"
        fi
    done

    # Also terminate any other instances in this VPC (e.g. manually launched ones)
    if [ -n "$vpc_id" ]; then
        local extra_instances
        extra_instances=$(aws ec2 describe-instances --region "$REGION" \
            --filters "Name=vpc-id,Values=${vpc_id}" "Name=instance-state-name,Values=running,stopped,pending" \
            --query 'Reservations[*].Instances[*].InstanceId' --output text 2>/dev/null || true)
        for inst_id in $extra_instances; do
            if ! echo "$all_instances" | grep -q "$inst_id"; then
                info "Terminating extra instance: ${inst_id}..."
                aws ec2 terminate-instances --region "$REGION" --instance-ids "$inst_id" >/dev/null 2>&1 || true
                all_instances="${all_instances} ${inst_id}"
            fi
        done
    fi

    if [ -n "$all_instances" ]; then
        info "Waiting for instances to terminate..."
        aws ec2 wait instance-terminated --region "$REGION" --instance-ids $all_instances 2>/dev/null || true
    fi

    # Delete key pair
    local key_name
    key_name=$(load_state "key_name")
    if [ -n "$key_name" ]; then
        info "Deleting key pair: ${key_name}..."
        aws ec2 delete-key-pair --region "$REGION" --key-name "$key_name" 2>/dev/null || true
    fi

    # Delete security groups (retry once -- ENIs can take a moment to detach)
    for sg_key in sg_server sg_node sg_client; do
        local sg_id
        sg_id=$(load_state "$sg_key")
        if [ -n "$sg_id" ]; then
            info "Deleting security group: ${sg_id}..."
            if ! aws ec2 delete-security-group --region "$REGION" --group-id "$sg_id" 2>/dev/null; then
                sleep 5
                if ! aws ec2 delete-security-group --region "$REGION" --group-id "$sg_id" 2>/dev/null; then
                    warn "Failed to delete security group ${sg_id}"
                    teardown_errors=$((teardown_errors + 1))
                fi
            fi
        fi
    done

    # Delete subnet
    local subnet_id
    subnet_id=$(load_state "subnet_id")
    if [ -n "$subnet_id" ]; then
        info "Deleting subnet: ${subnet_id}..."
        if ! aws ec2 delete-subnet --region "$REGION" --subnet-id "$subnet_id" 2>/dev/null; then
            warn "Failed to delete subnet ${subnet_id}"
            teardown_errors=$((teardown_errors + 1))
        fi
    fi

    # Disassociate and delete route table
    local rtb_id
    rtb_id=$(load_state "rtb_id")
    if [ -n "$rtb_id" ]; then
        local assoc_ids
        assoc_ids=$(aws ec2 describe-route-tables --region "$REGION" --route-table-ids "$rtb_id" \
            --query 'RouteTables[0].Associations[?Main!=`true`].RouteTableAssociationId' --output text 2>/dev/null || true)
        for assoc in $assoc_ids; do
            [ "$assoc" = "None" ] && continue
            info "Disassociating route table: ${assoc}..."
            aws ec2 disassociate-route-table --region "$REGION" --association-id "$assoc" 2>/dev/null || true
        done
        info "Deleting route table: ${rtb_id}..."
        if ! aws ec2 delete-route-table --region "$REGION" --route-table-id "$rtb_id" 2>/dev/null; then
            warn "Failed to delete route table ${rtb_id}"
            teardown_errors=$((teardown_errors + 1))
        fi
    fi

    # Detach and delete internet gateway
    local igw_id
    igw_id=$(load_state "igw_id")
    if [ -n "$igw_id" ] && [ -n "$vpc_id" ]; then
        info "Detaching internet gateway: ${igw_id}..."
        aws ec2 detach-internet-gateway --region "$REGION" --internet-gateway-id "$igw_id" --vpc-id "$vpc_id" 2>/dev/null || true
    fi
    if [ -n "$igw_id" ]; then
        info "Deleting internet gateway: ${igw_id}..."
        if ! aws ec2 delete-internet-gateway --region "$REGION" --internet-gateway-id "$igw_id" 2>/dev/null; then
            warn "Failed to delete internet gateway ${igw_id}"
            teardown_errors=$((teardown_errors + 1))
        fi
    fi

    # Delete VPC and verify
    if [ -n "$vpc_id" ]; then
        info "Deleting VPC: ${vpc_id}..."
        if ! aws ec2 delete-vpc --region "$REGION" --vpc-id "$vpc_id" 2>/dev/null; then
            warn "Failed to delete VPC ${vpc_id} -- checking for remaining dependencies..."
            local remaining
            remaining=$(aws ec2 describe-vpc-attribute --region "$REGION" --vpc-id "$vpc_id" --attribute enableDnsSupport 2>/dev/null && echo "VPC still exists" || echo "")
            if [ -n "$remaining" ]; then
                warn "VPC ${vpc_id} still exists. Manual cleanup required."
                warn "Run: aws ec2 describe-vpc-attribute --region $REGION --vpc-id $vpc_id"
                teardown_errors=$((teardown_errors + 1))
            fi
        fi
    fi

    # Full teardown: release EIP and delete data volumes
    if $full_teardown; then
        local eip_alloc
        eip_alloc=$(load_state "eip_alloc_id")
        if [ -n "$eip_alloc" ]; then
            info "Releasing Elastic IP: ${eip_alloc}..."
            aws ec2 release-address --region "$REGION" --allocation-id "$eip_alloc" 2>/dev/null || true
        fi

        for role in server node; do
            delete_volume "$role"
        done
        for cname in $cnames; do
            delete_volume "client_${cname}"
        done
    fi

    # Clean up local state
    local key_file
    key_file=$(load_state "key_file")
    if [ "$teardown_errors" -eq 0 ]; then
        if [ -n "$key_file" ] && [ -f "$key_file" ]; then
            rm -f "$key_file"
            info "Removed SSH key: ${key_file}"
        fi

        if $full_teardown; then
            rm -f "$STATE_FILE"
            info "Full teardown complete. All resources destroyed."
        else
            # Preserve persistent resource keys, remove everything else.
            # Per-client volumes (vol_client_<name>) are preserved dynamically.
            local preserved_keys="eip_alloc_id eip_public_ip vol_server vol_node az client_names"
            for cname in $cnames; do
                preserved_keys="${preserved_keys} vol_client_${cname}"
            done
            local tmp="${STATE_FILE}.tmp"
            python3 -c "
import json
with open('$STATE_FILE') as f:
    state = json.load(f)
keep = {k: state[k] for k in '$preserved_keys'.split() if k in state}
with open('$tmp', 'w') as f:
    json.dump(keep, f, indent=2)
"
            mv "$tmp" "$STATE_FILE"
            info "Teardown complete. Persistent resources preserved (EIP + data volumes)."
            info "  Re-deploy with: bash deploy/aws-deploy.sh deploy"
            info "  Full cleanup:   bash deploy/aws-deploy.sh teardown --full"
        fi
    else
        warn "Teardown completed with ${teardown_errors} error(s)."
        warn "State file preserved: ${STATE_FILE}"
        warn "Re-run 'teardown' after manually resolving the above issues."
        exit 1
    fi
}

# ---- argv wrappers for client-deploy / client-remove ----

# Parse the argv supplied to the `client-deploy` subcommand and call
# do_client_deploy with the resolved positional args.
do_client_deploy_cmd() {
    local name="" server_url="" join_token="" itype=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --client)        name="$2"; shift 2 ;;
            --server-url)    server_url="$2"; shift 2 ;;
            --join-token)    join_token="$2"; shift 2 ;;
            --instance-type) itype="$2"; shift 2 ;;
            *)               err "Unknown client-deploy option: $1" ;;
        esac
    done
    do_client_deploy "$name" "$server_url" "$join_token" "$itype"
}

do_client_remove_cmd() {
    parse_client_flag "$@"
    [ -n "$CLIENT_FLAG_NAME" ] || err "Usage: client-remove --client NAME"
    do_client_remove "$CLIENT_FLAG_NAME"
}

# ---- main ----

# Migrate any legacy single-client state keys before any subcommand reads them.
migrate_legacy_client_state

ACTION="${1:-}"
case "$ACTION" in
    deploy)        do_deploy ;;
    server-deploy) do_server_deploy ;;
    node-deploy)   do_node_deploy ;;
    client-deploy) shift; do_client_deploy_cmd "$@" ;;
    client-remove) shift; do_client_remove_cmd "$@" ;;
    teardown)      do_teardown "${2:-}" ;;
    status)        do_status ;;
    ssh)           shift; do_ssh "$@" ;;
    restart)       shift; do_restart "$@" ;;
    redeploy)      shift; do_redeploy "$@" ;;
    logs)          shift; do_logs "$@" ;;
    reload)        do_reload ;;
    test)          shift; do_test "$@" ;;
    openclaw)      shift; do_openclaw "$@" ;;
    "")            err "Usage: $0 <deploy|server-deploy|node-deploy|client-deploy|client-remove|teardown [--full]|status|ssh|redeploy|restart|logs|reload|test|openclaw>" ;;
    *)             err "Unknown action: $ACTION. Use: deploy, server-deploy, node-deploy, client-deploy, client-remove, teardown [--full], status, ssh, redeploy, restart, logs, reload, test, openclaw" ;;
esac
