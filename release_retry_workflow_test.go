package go_ci_action

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Exercise the actual workflow run blocks, including the normal tag path, with
// bounded API/Git fixtures. No real tag, network request or publisher is used.
func TestNativeReleaseRetryKeepsExactTagAndPublicationGuards(t *testing.T) {
	directory := t.TempDir()
	files := map[string]string{
		"recover-step.sh": releaseWorkflowRunBlock(t, "Recover this run's native release tag"),
		"tag-step.sh":     releaseWorkflowRunBlock(t, "Determine and push guarded tag"),
		"test_recovery.py": `import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

BASE = Path(__file__).parent
SHA = "c036941f2cb874c44a3152b7b5c773a979d24b0b"
TAG = "v0.136.0"


class Recovery(unittest.TestCase):
    def run_case(self, **changes):
        with tempfile.TemporaryDirectory(prefix="same-run-release-recovery-") as directory:
            directory = Path(directory)
            config = {"runs": [{"display_title": "native-release-41460412745-1", "event": "workflow_dispatch", "head_sha": SHA, "head_branch": TAG}], "release_status": 404, "release": {"message": "Not Found"}, "tag_sha": SHA}
            config.update(changes)
            fixture = directory / "case.json"
            fixture.write_text(json.dumps(config))
            gh = directory / "gh"
            gh.write_text('''#!/usr/bin/env python3
import json,os,sys
c=json.load(open(os.environ["CASE"]))
endpoint=sys.argv[-1]
if "/releases/tags/" in endpoint: status,body=c["release_status"],c["release"]
else: status,body=c.get("runs_status",200),{"workflow_runs":c["runs"],"total_count":c.get("runs_total",len(c["runs"]))}
if c.get("malformed_headers"): print("invalid response")
elif c.get("malformed_json"): print("HTTP/2.0 "+str(status)+" Fixture\\nContent-Type: application/json\\n\\n"+"not json")
else: print("HTTP/2.0 "+str(status)+" Fixture\\nContent-Type: application/json\\n\\n"+json.dumps(body))
sys.exit(0 if status==200 else 1)
''')
            git = directory / "git"
            git.write_text('''#!/usr/bin/env python3
import json,os,sys
if sys.argv[1:] != ["rev-parse","--verify","refs/tags/v0.136.0^{commit}"]: sys.exit("unexpected Git mutation or ref")
print(json.load(open(os.environ["CASE"]))["tag_sha"])
''')
            gh.chmod(0o700)
            git.chmod(0o700)
            output = directory / "output"
            env = os.environ.copy()
            env.update({"PATH": str(directory) + ":" + env["PATH"], "CASE": str(fixture), "REPO": "datatug/datatug-cli", "SHA": SHA, "WORKFLOW": "native-release.yml", "PREFIX": "v", "RUN_ID": "41460412745", "RUN_ATTEMPT": "2", "GITHUB_OUTPUT": str(output)})
            result = subprocess.run(["bash", str(BASE / "recover-step.sh")], env=env, text=True, capture_output=True, timeout=10)
            return result, output.read_text() if output.exists() else ""

    def test_unpublished_same_run_exact_tag_recovers(self):
        result, output = self.run_case()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output, "tag=" + TAG + "\n")

    def test_public_release_is_immutable_noop(self):
        result, output = self.run_case(release_status=200, release={"tag_name": TAG, "draft": False})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output, "already_published=true\n")

    def test_draft_refuses_automatic_overwrite(self):
        result, output = self.run_case(release_status=200, release={"tag_name": TAG, "draft": True})
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(output, "")

    def test_api_auth_failure_is_not_absence(self):
        result, output = self.run_case(release_status=403, release={"message": "Forbidden"})
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(output, "")

    def test_moved_tag_refuses(self):
        result, output = self.run_case(tag_sha="f" * 40)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(output, "")

    def test_foreign_parent_request_does_not_resume(self):
        result, output = self.run_case(runs=[{"display_title": "native-release-999-1", "event": "workflow_dispatch", "head_sha": SHA, "head_branch": TAG}])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output, "")

    def test_matching_request_wrong_source_refuses(self):
        result, output = self.run_case(runs=[{"display_title": "native-release-41460412745-1", "event": "workflow_dispatch", "head_sha": "f" * 40, "head_branch": TAG}])
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(output, "")

    def test_ambiguous_tags_refuse(self):
        runs = [{"display_title": "native-release-41460412745-1", "event": "workflow_dispatch", "head_sha": SHA, "head_branch": tag} for tag in [TAG, "v0.136.1"]]
        result, output = self.run_case(runs=runs)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(output, "")

    def test_same_or_future_attempt_does_not_resume(self):
        result, output = self.run_case(runs=[{"display_title": "native-release-41460412745-2", "event": "workflow_dispatch", "head_sha": SHA, "head_branch": TAG}])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output, "")


    def test_truncated_run_page_refuses(self):
        result, output = self.run_case(runs_total=2)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(output, "")

    def test_malformed_api_json_refuses(self):
        result, output = self.run_case(malformed_json=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(output, "")

    def test_malformed_api_headers_refuse(self):
        result, output = self.run_case(malformed_headers=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(output, "")

    def test_runs_api_failure_refuses(self):
        result, output = self.run_case(runs_status=503)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(output, "")

    def test_matching_request_wrong_event_refuses(self):
        result, output = self.run_case(runs=[{"display_title": "native-release-41460412745-1", "event": "push", "head_sha": SHA, "head_branch": TAG}])
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(output, "")

    def run_tag(self, recovered):
        with tempfile.TemporaryDirectory(prefix="same-run-tag-branch-") as directory:
            directory = Path(directory)
            git = directory / "git"
            git.write_text('''#!/usr/bin/env python3
import os,sys
with open(os.environ["GIT_CALLS"],"a") as log: log.write(" ".join(sys.argv[1:])+"\\n")
if sys.argv[1]=="rev-parse": print(os.environ["GITHUB_SHA"])
elif sys.argv[1:] not in [["tag","v0.136.1"],["push","origin","refs/tags/v0.136.1"]]: sys.exit("unexpected git call")
''')
            git.chmod(0o700)
            output = directory / "output"
            calls = directory / "calls"
            env = os.environ.copy()
            env.pop("RECOVERED_TAG", None)
            env.update({"PATH": str(directory)+":"+env["PATH"],"GITHUB_OUTPUT":str(output),"GIT_CALLS":str(calls),"GITHUB_SHA":SHA,"PREV":"0.136.0","NEXT":"0.136.1","NEXT_TAG":"v0.136.1","PREV_TAG":TAG,"ALLOW_MAJOR":"false","PREFIX":"v"})
            if recovered: env["RECOVERED_TAG"] = TAG
            result = subprocess.run(["bash", str(BASE/"tag-step.sh")],env=env,text=True,capture_output=True,timeout=10)
            return result, output.read_text() if output.exists() else "", calls.read_text() if calls.exists() else ""

    def test_recovered_tag_branch_never_mutates_git(self):
        result, output, calls = self.run_tag(True)
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(output,"new_tag="+TAG+"\n")
        self.assertEqual(calls,"rev-parse --verify refs/tags/"+TAG+"^{commit}\n")

    def test_normal_tag_branch_needs_no_new_environment(self):
        result, output, calls = self.run_tag(False)
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(output,"new_tag=v0.136.1\n")
        self.assertEqual(calls,"tag v0.136.1\npush origin refs/tags/v0.136.1\n")



if __name__ == "__main__":
    unittest.main(verbosity=2)
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("python3", filepath.Join(directory, "test_recovery.py"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("workflow retry regression: %v\n%s", err, output)
	}
}
