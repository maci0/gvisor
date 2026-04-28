load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hvf",
    srcs = [
        "address_space.go",
        "bluepill.go",
        "context.go",
        "hvf.go",
        "ipa_allocator.go",
        "kernel_pagetable.go",
        "machine.go",
        "memory_el1.go",
        "pagetable.go",
        "ring0_state.go",
        "runtime_el1.go",
        "sentry_pagetable.go",
        "syscall_el1.go",
        "tlbi_el1.go",
        "vcpu_arm64.go",
        "vmm_protocol.go",
    ],
    bazel_cgo = True,
    bazel_clinkopts = ["-framework Hypervisor"],
    stateify = False,
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/context",
        "//pkg/fd",
        "//pkg/hostarch",
        "//pkg/log",
        "//pkg/hosttid",
        "//pkg/sentry/arch",
        "//pkg/sentry/memmap",
        "//pkg/sentry/platform",
        "//pkg/sentry/platform/interrupt",
        "//pkg/sync",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "hvf_test",
    srcs = [
        "bluepill_test.go",
        "hvf_test.go",
        "ring0_test.go",
        "runtime_el1_test.go",
    ],
    library = ":hvf",
    deps = [
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
