genrule(
    name = "example-out",
    srcs = glob(["*.yaml.template"]) +
           [
               "addons.yaml",
               "//cmd/manager:manager-amd64.digest",
           ],
    outs = [
        "out/addons.yaml",
        "out/cluster.yaml",
        "out/machines.yaml",
        "out/aws_manager_image_patch.yaml",
    ],
    cmd = " ".join([
        "ENVSUBST=$(location @com_github_a8m_envsubst//cmd/envsubst:envsubst)",
        # "MANAGER_IMAGE=$$(cat $(location //cmd/manager:manager-amd64.digest))",
        # TODO(EKF): This is a hack. Figure out why the above digest line doesn't work.
        "MANAGER_IMAGE_PULL_POLICY=Never",
        "MANAGER_IMAGE=bazel/cmd/manager:manager-amd64",
        "OUTPUT_DIR=$(@D)/out",
        "DIR=.",
        "$(location :generate-yaml.sh) -f",
    ]),
    tools = [
        ":generate-yaml.sh",
        "@com_github_a8m_envsubst//cmd/envsubst:envsubst",
    ],
    visibility = ["//visibility:public"],
)
