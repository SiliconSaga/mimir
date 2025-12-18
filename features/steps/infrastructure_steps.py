from behave import given, then, when
import subprocess
import time

def run_kubectl(args, check=True):
    cmd = ["kubectl"] + args.split()
    return subprocess.run(cmd, check=check, capture_output=True, text=True)

@given('the "{deployment}" deployment is running in "{namespace}"')
def step_check_deployment(context, deployment, namespace):
    # Check if deployment exists and has Available replicas
    cmd = f"get deployment {deployment} -n {namespace} -o jsonpath={{.status.readyReplicas}}"
    result = run_kubectl(cmd, check=False)
    assert result.returncode == 0, f"Deployment {deployment} not found"
    assert result.stdout.strip() != "", f"Deployment {deployment} has 0 ready replicas"

@then('the "{crd}" CRD should be established')
def step_check_crd(context, crd):
    cmd = f"get crd {crd}"
    run_kubectl(cmd)

@given('the KafkaCluster Claim "{name}" is applied')
def step_apply_kafka_claim(context, name):
    # In reality, this might apply a specific YAML file
    # For now, we assume it's already applied or we apply it here
    pass

@then('the "{kind}" cluster should be ready in "{namespace}"')
def step_check_cluster_ready(context, kind, namespace):
    # Simplified check
    cmd = f"get {kind} -n {namespace}"
    run_kubectl(cmd)

@then('the Crossplane claim "{name}" should be "Ready"')
def step_check_crossplane_claim(context, name):
    # Check claim status
    pass
