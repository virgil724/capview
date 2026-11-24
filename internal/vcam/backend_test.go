package vcam

import "testing"

func TestParseBackend(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Backend
		wantErr bool
	}{
		{"", BackendAuto, false},
		{"auto", BackendAuto, false},
		{"MF", BackendMF, false},
		{"dshow", BackendDShow, false},
		{"DirectShow", BackendDShow, false},
		{"both", BackendBoth, false},
		{"invalid", "", true},
	} {
		got, err := ParseBackend(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseBackend(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseBackend(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseRuntimeBackend(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Backend
		wantErr bool
	}{
		{"", BackendAuto, false},
		{"auto", BackendAuto, false},
		{"mf", BackendMF, false},
		{"dshow", BackendDShow, false},
		{"both", "", true},
		{"bogus", "", true},
	} {
		got, err := ParseRuntimeBackend(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseRuntimeBackend(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseRuntimeBackend(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSelectExpectedBackend(t *testing.T) {
	tests := []struct {
		name        string
		build       uint32
		mfSupported bool
		want        Backend
	}{
		{"win11_22000_mf_ok", 22000, true, BackendMF},
		{"win11_26100_mf_ok", 26100, true, BackendMF},
		{"win11_mf_unsupported", 22631, false, BackendDShow},
		{"win10_19045_no_mf", 19045, false, BackendDShow},
		{"win10_19045_even_if_mf_flag_true", 19045, true, BackendDShow},
		{"win10_10240", 10240, false, BackendDShow},
		{"unknown_build_0", 0, false, BackendDShow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SelectExpectedBackend(tc.build, tc.mfSupported); got != tc.want {
				t.Fatalf("SelectExpectedBackend(%d, %v) = %q, want %q", tc.build, tc.mfSupported, got, tc.want)
			}
		})
	}
}

func TestEvaluateInstallStatus(t *testing.T) {
	base := func() StatusInput {
		return StatusInput{
			InstalledPath:    InstalledDLLPath,
			InstalledExists:  true,
			InstalledVersion: "1.1.0.0",
			InstalledSHA256:  "aaa",
			BundledPath:      `C:\app\capview_vcam.dll`,
			BundledExists:    true,
			BundledVersion:   "1.1.0.0",
			BundledSHA256:    "aaa",
		}
	}

	t.Run("not_installed_win11", func(t *testing.T) {
		in := base()
		in.WindowsBuild = 26100
		in.MFSupported = true
		in.InstalledExists = false
		st := EvaluateInstallStatus(in)
		if st.Installed || st.AnyRegistered || st.Mismatch || st.ExpectedBackend != BackendMF || st.MismatchReason != MismatchNotInstalled {
			t.Fatalf("unexpected status: %+v", st)
		}
		if !st.NeedsInstallOrReinstall() || st.MenuEnableLabel() != "Enable" {
			t.Fatalf("unexpected menu/needs state: needs=%v label=%q", st.NeedsInstallOrReinstall(), st.MenuEnableLabel())
		}
	})

	t.Run("valid_win11_mf", func(t *testing.T) {
		in := base()
		in.WindowsBuild = 26100
		in.MFSupported = true
		in.MFKeyExists = true
		in.MFValid = true
		in.MFRegisteredPath = InstalledDLLPath
		st := EvaluateInstallStatus(in)
		if !st.Installed || !st.UpToDate || st.Mismatch || st.ExpectedBackend != BackendMF || st.RegisteredBackend != BackendMF {
			t.Fatalf("unexpected status: %+v", st)
		}
		if st.NeedsInstallOrReinstall() {
			t.Fatal("should not need install/reinstall")
		}
	})

	t.Run("valid_win10_dshow", func(t *testing.T) {
		in := base()
		in.WindowsBuild = 19045
		in.MFSupported = false
		in.DShowCLSIDExists = true
		in.DShowCLSIDValid = true
		in.DShowCategoryRegistered = true
		in.DShowRegisteredPath = InstalledDLLPath
		st := EvaluateInstallStatus(in)
		if !st.Installed || !st.UpToDate || st.Mismatch || st.ExpectedBackend != BackendDShow || st.RegisteredBackend != BackendDShow {
			t.Fatalf("unexpected status: %+v", st)
		}
		if st.NeedsInstallOrReinstall() {
			t.Fatal("should not need install/reinstall")
		}
	})

	t.Run("mismatch_win10_to_win11_upgrade_stale_dshow", func(t *testing.T) {
		in := base()
		in.WindowsBuild = 22631
		in.MFSupported = true
		in.DShowCLSIDExists = true
		in.DShowCLSIDValid = true
		in.DShowCategoryRegistered = true
		st := EvaluateInstallStatus(in)
		if st.Installed || !st.AnyRegistered || !st.Mismatch || st.MismatchReason != MismatchOSBackendChanged {
			t.Fatalf("expected OS backend mismatch: %+v", st)
		}
		if !st.NeedsInstallOrReinstall() || st.MenuEnableLabel() != "Reinstall (OS Changed)" {
			t.Fatalf("unexpected menu prompt: needs=%v label=%q", st.NeedsInstallOrReinstall(), st.MenuEnableLabel())
		}
	})

	t.Run("mismatch_win10_with_stale_mf", func(t *testing.T) {
		in := base()
		in.WindowsBuild = 19045
		in.MFSupported = false
		in.MFKeyExists = true
		in.MFValid = true
		st := EvaluateInstallStatus(in)
		if st.Installed || !st.Mismatch || st.MismatchReason != MismatchOSBackendChanged {
			t.Fatalf("expected OS backend mismatch on Win10 with MF: %+v", st)
		}
		if st.MenuEnableLabel() != "Reinstall (OS Changed)" {
			t.Fatalf("label = %q, want Reinstall (OS Changed)", st.MenuEnableLabel())
		}
	})

	t.Run("mismatch_dual_registration_on_win11", func(t *testing.T) {
		in := base()
		in.WindowsBuild = 26100
		in.MFSupported = true
		in.MFKeyExists = true
		in.MFValid = true
		in.DShowCLSIDExists = true
		in.DShowCLSIDValid = true
		in.DShowCategoryRegistered = true
		st := EvaluateInstallStatus(in)
		if st.Installed || !st.Mismatch || st.MismatchReason != MismatchDualRegistered || st.RegisteredBackend != BackendBoth {
			t.Fatalf("expected dual registration mismatch: %+v", st)
		}
		if st.MenuEnableLabel() != "Reinstall (OS Changed)" {
			t.Fatalf("label = %q, want Reinstall (OS Changed)", st.MenuEnableLabel())
		}
	})

	t.Run("mismatch_dshow_missing_category", func(t *testing.T) {
		in := base()
		in.WindowsBuild = 19045
		in.MFSupported = false
		in.DShowCLSIDExists = true
		in.DShowCLSIDValid = true
		in.DShowCategoryRegistered = false
		st := EvaluateInstallStatus(in)
		if st.Installed || !st.Mismatch || st.MismatchReason != MismatchPartialRegistration {
			t.Fatalf("expected partial registration mismatch: %+v", st)
		}
		if st.MenuEnableLabel() != "Enable (Reinstall Required)" {
			t.Fatalf("label = %q, want Enable (Reinstall Required)", st.MenuEnableLabel())
		}
	})

	t.Run("mismatch_missing_dll_on_disk", func(t *testing.T) {
		in := base()
		in.WindowsBuild = 19045
		in.DShowCLSIDExists = true
		in.DShowCLSIDValid = true
		in.DShowCategoryRegistered = true
		in.InstalledExists = false
		st := EvaluateInstallStatus(in)
		if st.Installed || !st.AnyRegistered || !st.Mismatch || st.MismatchReason != MismatchDLLMissing {
			t.Fatalf("expected missing DLL mismatch: %+v", st)
		}
		if st.MenuEnableLabel() != "Enable (Reinstall Required)" {
			t.Fatalf("label = %q", st.MenuEnableLabel())
		}
	})

	t.Run("mismatch_dll_outdated", func(t *testing.T) {
		in := base()
		in.WindowsBuild = 26100
		in.MFSupported = true
		in.MFKeyExists = true
		in.MFValid = true
		in.InstalledVersion = "1.0.0.0"
		in.InstalledSHA256 = "oldhash"
		st := EvaluateInstallStatus(in)
		if !st.Installed || st.UpToDate || !st.Mismatch || st.MismatchReason != MismatchDLLOutdated {
			t.Fatalf("expected outdated DLL status: %+v", st)
		}
		if !st.NeedsInstallOrReinstall() {
			t.Fatal("outdated DLL must trigger upgrade on enable")
		}
	})

	t.Run("explicit_dshow_override_on_win11", func(t *testing.T) {
		in := base()
		in.WindowsBuild = 26100
		in.MFSupported = true
		in.RequestedBackend = BackendDShow
		in.DShowCLSIDExists = true
		in.DShowCLSIDValid = true
		in.DShowCategoryRegistered = true
		st := EvaluateInstallStatus(in)
		if !st.Installed || !st.UpToDate || st.Mismatch || st.ExpectedBackend != BackendDShow {
			t.Fatalf("explicit dshow override on Win11 should succeed: %+v", st)
		}
	})
}
