# Description:
#   This VDSO is a shared library that provides the same interfaces as the
#   normal system VDSO (time, gettimeofday, clock_gettimeofday) but which uses
#   timekeeping parameters managed by the sandbox kernel.

# Placeholder: load py_test
load("//tools:arch.bzl", "select_arch")
load("//tools:defs.bzl", "cc_flags_supplier", "cc_toolchain")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

exports_files(["check_vdso.py"])

genrule(
    name = "vdso",
    srcs = [
        "barrier.h",
        "compiler.h",
        "cycle_clock.h",
        "seqlock.h",
        "syscalls.h",
        "vdso.cc",
        "vdso_amd64.lds",
        "vdso_arm64.lds",
        "vdso_time.h",
        "vdso_time.cc",
    ],
    outs = [
        "vdso.so",
    ],
    cmd = "$(CC) $(CC_FLAGS) " +
          "-I. " +
          "-O2 " +
          "-std=c++11 " +
          "-fPIC " +
          "-fno-sanitize=all " +
          # Some toolchains enable stack protector by default. Disable it, the
          # VDSO has no hooks to handle failures.
          "-fno-stack-protector " +
          "-shared " +
          "-nostdlib " +
          "-Wl,-soname=linux-vdso.so.1 " +
          "-Wl,--hash-style=sysv " +
          "-Wl,--no-undefined " +
          "-Wl,-Bsymbolic " +
          "-Wl,-z,max-page-size=4096 " +
          "-Wl,-z,common-page-size=4096 " +
          select_arch(
              amd64 = "-Wl,-T$(location vdso_amd64.lds) ",
              arm64 = "-Wl,-T$(location vdso_arm64.lds) ",
              no_match_error = "unsupported architecture",
          ) +
          "-o $(location vdso.so) " +
          "$(location vdso.cc) " +
          "$(location vdso_time.cc)",
    features = ["-pie"],
    target_compatible_with = ["@platforms//os:linux"],
    toolchains = [
        cc_toolchain,
        ":no_pie_cc_flags",
    ],
    visibility = ["//:sandbox"],
)

# On macOS, produce an empty VDSO stub. The sentry will skip VDSO
# loading when it detects an empty/invalid binary.
genrule(
    name = "vdso_stub",
    outs = ["vdso_stub.so"],
    cmd = "touch $(location vdso_stub.so)",
    target_compatible_with = ["@platforms//os:macos"],
    visibility = ["//:sandbox"],
)

cc_flags_supplier(
    name = "no_pie_cc_flags",
    features = ["-pie"],
)

py_test(
    name = "vdso_test",
    srcs = ["check_vdso.py"],
    args = [
        "--check-data",
        "--vdso=$(location :vdso)",
    ],
    data = [":vdso"],
    main = "check_vdso.py",
)
