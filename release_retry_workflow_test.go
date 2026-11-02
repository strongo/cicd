package go_ci_action

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
            config.setdefault("head_tags", [])
            config.setdefault("tags", [TAG])
            config.setdefault("release_pages", [[changes["release"]]] if changes.get("release_status")==200 else [[]])
            config.setdefault("list_status", changes["release_status"] if changes.get("release_status") in [401,403,500,503] else 200)
            fixture = directory / "case.json"
            fixture.write_text(json.dumps(config))
            gh = directory / "gh"
            gh.write_text('#!/usr/bin/env python3\nimport json,os,sys\nc=json.load(open(os.environ["CASE"]))\nendpoint=sys.argv[-1]\nif "/releases?" in endpoint:\n if "--paginate" not in sys.argv or "--slurp" not in sys.argv: sys.exit("release listing must be complete")\n status=c.get("list_status",200)\n print("not json" if c.get("malformed_list_json") else json.dumps(c["release_pages"]))\n sys.exit(0 if status==200 else 1)\nelif "/releases/tags/" in endpoint: status,body=404,{"message":"Not Found"}\nelse: status,body=c.get("runs_status",200),{"workflow_runs":c["runs"],"total_count":c.get("runs_total",len(c["runs"]))}\nif c.get("malformed_headers"): print("invalid response")\nelif c.get("malformed_json"): print("HTTP/2.0 "+str(status)+" Fixture\\nContent-Type: application/json\\n\\nnot json")\nelse: print("HTTP/2.0 "+str(status)+" Fixture\\nContent-Type: application/json\\n\\n"+json.dumps(body))\nsys.exit(0 if status==200 else 1)\n')
            git = directory / "git"
            git.write_text('#!/usr/bin/env python3\nimport json,os,sys\nc=json.load(open(os.environ["CASE"]))\na=sys.argv[1:]\nif a[:2]==["rev-parse","--verify"] and len(a)==3: print(c["tag_sha"])\nelif a[:2]==["tag","--points-at"] and len(a)==4 and a[-1]=="--list": print("\\n".join(c["head_tags"]))\nelif a==["tag","--list"]: print("\\n".join(c["tags"]))\nelse: sys.exit("unexpected Git mutation or ref")\n')
            gh.chmod(0o700)
            git.chmod(0o700)
            output = directory / "output"
            env = os.environ.copy()
            env.update({"PATH": str(directory) + ":" + env["PATH"], "CASE": str(fixture), "REPO": "datatug/datatug-cli", "SHA": SHA, "WORKFLOW": "native-release.yml", "PREFIX": "v", "RUN_ID": "41460412745", "RUN_ATTEMPT": str(config.get("parent_attempt",2)), "GITHUB_OUTPUT": str(output)})
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

    def run_tag(self, recovered, mismatch=False):
        with tempfile.TemporaryDirectory(prefix="same-run-tag-branch-") as directory:
            directory = Path(directory)
            git = directory / "git"
            git.write_text('''#!/usr/bin/env python3
import os,sys
with open(os.environ["GIT_CALLS"],"a") as log: log.write(" ".join(sys.argv[1:])+"\\n")
if sys.argv[1]=="rev-parse": print(os.environ.get("TAG_SHA",os.environ["GITHUB_SHA"]))
elif sys.argv[1:] not in [["tag","v0.136.1"],["push","origin","refs/tags/v0.136.1"]]: sys.exit("unexpected git call")
''')
            git.chmod(0o700)
            output = directory / "output"
            calls = directory / "calls"
            env = os.environ.copy()
            env.pop("RECOVERED_TAG", None)
            env.update({"PATH": str(directory)+":"+env["PATH"],"GITHUB_OUTPUT":str(output),"GIT_CALLS":str(calls),"GITHUB_SHA":SHA,"PREV":"0.136.0","NEXT":"0.136.1","NEXT_TAG":"v0.136.1","PREV_TAG":TAG,"ALLOW_MAJOR":"false","PREFIX":"v"})
            if recovered: env["RECOVERED_TAG"] = TAG
            if mismatch: env["TAG_SHA"] = "f"*40
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



    def test_draft_only_in_second_list_page_refuses_despite_tag404(self):
        result, output=self.run_case(release_pages=[[{"tag_name":"v0.135.4","draft":False}],[{"tag_name":TAG,"draft":True}]])
        self.assertNotEqual(result.returncode,0)
        self.assertEqual(output,"")

    def test_duplicate_matching_release_refuses(self):
        result,output=self.run_case(release_pages=[[{"tag_name":TAG,"draft":False}],[{"tag_name":TAG,"draft":False}]])
        self.assertNotEqual(result.returncode,0)
        self.assertEqual(output,"")

    def test_malformed_release_pages_refuse(self):
        for pages in [{"tag_name":TAG},[{}],[[{"tag_name":TAG,"draft":"false"}]],[]]:
            result,output=self.run_case(release_pages=pages)
            self.assertNotEqual(result.returncode,0)
            self.assertEqual(output,"")

    def test_malformed_release_json_refuses(self):
        result,output=self.run_case(malformed_list_json=True)
        self.assertNotEqual(result.returncode,0)
        self.assertEqual(output,"")

    def test_head_tag_without_receipt_refuses_red(self):
        result,output=self.run_case(runs=[],head_tags=[TAG])
        self.assertNotEqual(result.returncode,0)
        self.assertIn("no prior native request",result.stderr)
        self.assertEqual(output,"")

    def test_public_head_without_receipt_is_immutable_noop(self):
        result,output=self.run_case(runs=[],head_tags=[TAG],release_pages=[[{"tag_name":TAG,"draft":False}]])
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(output,"already_published=true\n")

    def test_newer_tag_anywhere_blocks_late_recovery(self):
        result,output=self.run_case(tags=[TAG,"v0.136.1"])
        self.assertNotEqual(result.returncode,0)
        self.assertEqual(output,"")

    def test_numeric_versions_not_lexical(self):
        result,output=self.run_case(runs=[{"display_title":"native-release-41460412745-1","event":"workflow_dispatch","head_sha":SHA,"head_branch":"v0.9.0"}],tags=["v0.9.0","v0.10.0"])
        self.assertNotEqual(result.returncode,0)
        self.assertEqual(output,"")

    def test_other_prefix_tag_does_not_block(self):
        result,output=self.run_case(tags=[TAG,"other/v999.0.0"])
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(output,"tag="+TAG+"\n")

    def test_same_tag_multiple_prior_attempts_recover(self):
        result,output=self.run_case(parent_attempt=3,runs=[{"display_title":"native-release-41460412745-"+str(attempt),"event":"workflow_dispatch","head_sha":SHA,"head_branch":TAG} for attempt in [1,2]])
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(output,"tag="+TAG+"\n")

    def test_two_head_tags_newer_blocks_older(self):
        result,output=self.run_case(head_tags=[TAG,"v0.136.1"],tags=[TAG,"v0.136.1"])
        self.assertNotEqual(result.returncode,0)
        self.assertEqual(output,"")

    def test_two_head_tags_lower_does_not_change_selected_tag(self):
        result,output=self.run_case(head_tags=[TAG,"v0.135.9"],tags=[TAG,"v0.135.9"])
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(output,"tag="+TAG+"\n")

    def test_recovered_tag_step_rechecks_sha(self):
        result,output,calls=self.run_tag(True,mismatch=True)
        self.assertNotEqual(result.returncode,0)
        self.assertEqual(output,"")
        self.assertNotIn("push",calls)
        self.assertNotIn("tag v",calls)


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

// The effective tag must be identical at native build and publication, even
// when another tag points at HEAD. Keep the event/step handoff fail-closed.
func TestNativeReleaseRetryWiringPinsPublisherTag(t *testing.T) {
	workflow := readReleaseWorkflow(t)
	section := func(name string) string {
		marker := "      - name: " + name + "\n"
		at := strings.Index(workflow, marker)
		if at < 0 {
			t.Fatalf("missing step %s", name)
		}
		remaining := workflow[at+len(marker):]
		if next := strings.Index(remaining, "      - name:"); next >= 0 {
			return remaining[:next]
		}
		return remaining
	}
	for _, want := range []string{"github.run_attempt > 1", "inputs.native_build_workflow != ''", "RUN_ID: ${{ github.run_id }}", "RUN_ATTEMPT: ${{ github.run_attempt }}", "SHA: ${{ github.sha }}", "python3 -I"} {
		if !strings.Contains(section("Recover this run's native release tag"), want) {
			t.Fatalf("recovery missing %s", want)
		}
	}
	for _, want := range []string{"steps.retry_native_tag.outputs.tag == ''", "steps.retry_native_tag.outputs.already_published != 'true'"} {
		if !strings.Contains(section("Resolve configured default bump"), want) {
			t.Fatalf("bump may overwrite retry result: %s", want)
		}
	}
	if !strings.Contains(section("Determine and push guarded tag"), "RECOVERED_TAG: ${{ steps.retry_native_tag.outputs.tag }}") {
		t.Fatal("recovered tag handoff missing")
	}
	effective := "${{ steps.tag.outputs.new_tag || github.ref_name }}"
	if !strings.Contains(section("Require native release builds at the exact tag"), "TAG: "+effective) {
		t.Fatal("native gate tag changed")
	}
	if !strings.Contains(section("Run GoReleaser"), "GORELEASER_CURRENT_TAG: "+effective) {
		t.Fatal("publisher not pinned to guarded/native tag")
	}
}
