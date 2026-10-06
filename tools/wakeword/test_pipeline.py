"""Offline regression tests; fixture licence claims are not real data approvals."""

import ast
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock

from lint_licences import HERE, inventory, validate
from train import preflight


class ProvenanceTests(unittest.TestCase):
    def setUp(self):
        self.entry = {
            'id': 'fixture', 'kind': 'dataset', 'licence': 'CC0-1.0',
            'source': 'https://example.invalid/fixture-v1',
            'licence_source': 'https://example.invalid/fixture-v1/licence',
            'sha256': hashlib.sha256(b'fixture').hexdigest(),
            'path': 'fixture.bin', 'attribution': 'Test fixture only',
        }
        self.recipe = {'schema_version': 1, 'inputs': ['fixture'],
                       'image': 'example.invalid/trainer@sha256:' + 'a' * 64}

    def document(self, entries=None):
        return '# Inputs\n\n```json\n' + json.dumps(
            [self.entry] if entries is None else entries) + '\n```\n'

    def test_permissive_record_and_checksum(self):
        with tempfile.TemporaryDirectory() as data:
            Path(data, 'fixture.bin').write_bytes(b'fixture')
            self.assertEqual(len(preflight(self.recipe, self.document(), data)), 1)
            Path(data, 'fixture.bin').write_bytes(b'changed')
            with self.assertRaisesRegex(ValueError, 'changed'):
                validate(self.document(), self.recipe, data)

    def test_reject_restricted_unknown_and_composite_licences(self):
        for licence in ('CC-BY-NC-SA-4.0', 'CC-BY-NC-4.0', 'Proprietary',
                        'unknown', 'MIT OR Proprietary', 'MIT AND CC-BY-NC-4.0', ''):
            with self.subTest(licence=licence):
                self.entry['licence'] = licence
                with self.assertRaises(ValueError):
                    inventory(self.document())

    def test_empty_and_malformed_inventory(self):
        for document in (self.document([]), '', self.document() * 2,
                         '```json\n{}\n```', '```json\n[null]\n```',
                         '```json\n[\n```'):
            with self.subTest(document=document), self.assertRaises(ValueError):
                inventory(document)

    def test_missing_field_duplicate_and_invalid_hash(self):
        with self.assertRaises(ValueError):
            inventory(self.document([self.entry, self.entry]))
        del self.entry['attribution']
        with self.assertRaises(ValueError):
            inventory(self.document())
        self.entry['attribution'] = 'fixture'
        self.entry['sha256'] = 'unverified'
        with self.assertRaises(ValueError):
            inventory(self.document())

    def test_recipe_cannot_omit_or_add_inputs(self):
        for recipe in (None, [], "invalid"):
            with self.subTest(recipe=recipe), self.assertRaises(ValueError):
                validate(self.document(), recipe)
            with self.subTest(recipe=recipe), self.assertRaises(ValueError):
                preflight(recipe, self.document(), None)
        for inputs in ([], ['unlisted'], ['fixture', 'fixture'], [None]):
            with self.subTest(inputs=inputs), self.assertRaises(ValueError):
                validate(self.document(), {'inputs': inputs})

    def test_paths_cannot_escape(self):
        for path in ('../outside', '/outside', 'a/../../outside', 'a\\outside', 'a//b'):
            self.entry['path'] = path
            with self.subTest(path=path), self.assertRaises(ValueError):
                inventory(self.document())
        self.entry['path'] = 'fixture.bin'
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp, 'data')
            root.mkdir()
            outside = Path(tmp, 'outside')
            outside.write_bytes(b'fixture')
            (root / 'fixture.bin').symlink_to(outside)
            with self.assertRaisesRegex(ValueError, 'inside'):
                validate(self.document(), self.recipe, root)

    def test_no_floating_container_tag(self):
        for image in (None, 'python:latest', 'trainer:1', 'trainer@sha256:bad'):
            self.recipe['image'] = image
            with self.subTest(image=image), self.assertRaisesRegex(ValueError, 'digest'):
                preflight(self.recipe, self.document(), None)

    def test_shipped_recipe_is_reviewed_but_unreleased(self):
        recipe = json.loads((HERE / 'recipe.yaml').read_text())
        document = (HERE / 'DATA-LICENCES.md').read_text()
        self.assertEqual(recipe['status'], 'trained-unreleased')
        self.assertTrue(recipe['inputs'])
        self.assertEqual(len(validate(document, recipe)), len(recipe['inputs']))
        # no published image yet: the container route stays refused, the locked venv runs
        with self.assertRaisesRegex(ValueError, 'digest'):
            preflight(recipe, document, None)
        self.assertTrue(preflight(recipe, document, None, local=True))
        names = [m['name'] for m in recipe['models']]
        self.assertEqual(names, ['hey_mirrin', 'hey_nyra', 'hey_pickoo'])

    def test_driver_runs_nothing_on_dry_run_and_needs_image_or_local(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / 'fixture.bin').write_bytes(b'fixture')
            recipe = dict(self.recipe, image=None,
                          models=[{'name': 'hey_fixture', 'train_args': ['--steps', 5]}])
            (root / 'recipe.yaml').write_text(json.dumps(recipe))
            (root / 'licences.md').write_text(self.document())
            command = [sys.executable, '-B', str(HERE / 'train.py'),
                       '--recipe', str(root / 'recipe.yaml'),
                       '--licences', str(root / 'licences.md'), '--data', tmp]
            refused = subprocess.run(command + ['--dry-run'], capture_output=True, text=True)
            self.assertEqual(refused.returncode, 1)
            self.assertIn('digest', refused.stderr)
            dry = subprocess.run(command + ['--local', '--dry-run'], capture_output=True, text=True)
            self.assertEqual(dry.returncode, 0, dry.stderr)
            self.assertIn('train-hey_fixture', dry.stdout)
            self.assertIn('train_model.py hey_fixture --seed 7 --steps 5', dry.stdout)
            self.assertIn('Dry run', dry.stdout)
            unknown = subprocess.run(command + ['--local', '--dry-run', '--from', 'nope'],
                                     capture_output=True, text=True)
            self.assertEqual(unknown.returncode, 1)
            self.assertFalse(list(root.glob('*.onnx')))

    def test_pipeline_never_downloads(self):
        # Inputs are fetched only by the explicit, reviewed fetch_inputs.sh.
        for path in (HERE / 'pipeline').glob('*.py'):
            text = path.read_text()
            for needle in ('download_models', 'urllib', 'requests.', 'http://', 'https://'):
                if path.name == 'make_inventory.py' and needle == 'https://':
                    continue  # provenance URLs in the inventory table, never fetched
                with self.subTest(path=path.name, needle=needle):
                    self.assertNotIn(needle, text)

    def test_phrases_keep_positives_and_near_misses_apart(self):
        namespace = {}
        exec((HERE / 'pipeline' / 'phrases.py').read_text(), namespace)
        norm = lambda t: ''.join(c for c in t.lower() if c.isalnum() or c == ' ').strip()
        models = namespace['MODELS']
        self.assertEqual(sorted(models), ['hey_mirrin', 'hey_nyra', 'hey_pickoo'])
        for name, cfg in models.items():
            positives = {norm(t) for t in cfg['positive_text'] + cfg['positive_rare']}
            negatives = {norm(t) for t in cfg['near_miss'] + cfg['eval_near_miss']}
            negatives |= {norm('hey ' + n) for n in cfg['hey_near']}
            with self.subTest(model=name):
                self.assertFalse(positives & negatives)
                self.assertNotIn('maverick', ' '.join(positives | negatives))

    def test_planned_empty_inventory_requires_explicit_cli_flag(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            recipe = root / 'recipe.yaml'
            licences = root / 'licences.md'
            licences.write_text(self.document([]))
            recipe.write_text(json.dumps({'status': 'planned', 'inputs': []}))
            command = [sys.executable, '-B', str(HERE / 'lint_licences.py'),
                       '--recipe', str(recipe), '--licences', str(licences)]
            strict = subprocess.run(command, capture_output=True, text=True)
            self.assertEqual(strict.returncode, 1)
            planned = subprocess.run(command + ['--allow-empty-planned'],
                                     capture_output=True, text=True)
            self.assertEqual(planned.returncode, 0, planned.stderr)
            self.assertIn('Training and release remain blocked', planned.stdout)
            for status in ('ready', 'released', None):
                recipe.write_text(json.dumps({'status': status, 'inputs': []}))
                result = subprocess.run(command + ['--allow-empty-planned'],
                                        capture_output=True, text=True)
                self.assertEqual(result.returncode, 1, status)

    def test_planned_flag_does_not_bypass_input_validation(self):
        recipe = {'status': 'planned', 'inputs': []}
        for document in ('', '```json\n{}\n```', '```json\n[\n```'):
            with self.subTest(document=document), self.assertRaises(ValueError):
                validate(document, recipe, allow_empty_planned=True)
        recipe['inputs'] = ['fixture']
        with self.assertRaises(ValueError):
            validate(self.document([]), recipe, allow_empty_planned=True)
        for licence in ('CC-BY-NC-SA-4.0', 'Proprietary', 'unknown'):
            self.entry['licence'] = licence
            with self.subTest(licence=licence), self.assertRaises(ValueError):
                validate(self.document(), recipe, allow_empty_planned=True)
        self.entry['licence'] = 'MIT'
        with tempfile.TemporaryDirectory() as tmp:
            Path(tmp, 'fixture.bin').write_bytes(b'changed')
            with self.assertRaisesRegex(ValueError, 'changed'):
                validate(self.document(), recipe, tmp, allow_empty_planned=True)
        with self.assertRaisesRegex(ValueError, 'No training inputs'):
            preflight(dict(self.recipe, status='planned', inputs=[]), self.document([]), None)

    def test_legacy_synthesis_remains_available_without_running_voices(self):
        docs = HERE.parent.parent / 'docs'
        # Compile all helpers without importing dependencies, touching the twin's
        # home, or invoking a voice. Exercise the clip converter with fake I/O.
        for name in ('gen_clips', 'gen_extra', 'gen_adv', 'gen_mix', 'eval'):
            path = docs / f'wake-word-{name}.py'
            compile(path.read_text(), str(path), 'exec')
        source = ast.parse((docs / 'wake-word-gen_clips.py').read_text())
        clip = next(node for node in source.body
                    if isinstance(node, ast.FunctionDef) and node.name == 'say_clip')
        process, filesystem = Mock(), Mock()
        namespace = {'subprocess': process, 'os': filesystem}
        exec(compile(ast.Module(body=[clip], type_ignores=[]), '<clip>', 'exec'), namespace)
        namespace['say_clip']('Hey Nova', 'Fixture voice', 180, 'fixture.wav')
        process.run.assert_any_call(
            ['say', '-v', 'Fixture voice', '-r', '180', '-o', 'fixture.wav.aiff', 'Hey Nova'],
            check=True,
        )
        process.run.assert_any_call(
            ['sox', '-q', 'fixture.wav.aiff', '-r', '16000', '-c', '1', '-b', '16', 'fixture.wav'],
            check=True,
        )
        filesystem.remove.assert_called_once_with('fixture.wav.aiff')
        config = (docs / 'wake-word-config.yaml').read_text()
        self.assertIn('feature_data_files:', config)
        self.assertIn('ACAV100M_sample:', config)


def _have_pipeline_deps():
    try:
        import numpy, scipy, soundfile  # noqa: F401
    except ImportError:
        return False
    return True


@unittest.skipUnless(_have_pipeline_deps(), 'needs the pipeline virtualenv (numpy, scipy, soundfile)')
class StreamFeatureTests(unittest.TestCase):
    """Regression: long recordings were decoded and embedded whole, and every embedding was
    held in RAM, which exhausted memory on 17-minute MUSAN files and crashed the laptop."""

    def setUp(self):
        sys.path.insert(0, str(HERE / 'pipeline'))
        import features
        self.f = features
        self.tmp = Path(tempfile.mkdtemp())

    def test_long_recordings_are_read_and_embedded_in_bounded_chunks(self):
        import numpy as np
        import soundfile as sf
        f = self.f
        secs = 150
        sf.write(str(self.tmp / 'long.wav'), np.random.default_rng(1).uniform(-.5, .5, secs * f.SR)
                 .astype(np.float32), f.SR, subtype='PCM_16')
        reads, embedded = [], []
        real_read = f.read_span

        def read_span(path, start=0, n=None):
            reads.append(n)
            return real_read(path, start, n)

        fake = Mock()
        fake._get_embeddings.side_effect = lambda pcm: (embedded.append(len(pcm)),
                                                       np.zeros((len(pcm) // 1280 - 15, 96)))[1]
        with unittest.mock.patch.object(f, 'X', self.tmp), \
                unittest.mock.patch.object(f, 'read_span', read_span), \
                unittest.mock.patch.object(f, 'fe', lambda: fake):
            out = f.stream_worker(('long.wav', 'train', False, 3))
        self.assertTrue(reads and all(n is not None and n <= f.CHUNK for n in reads))
        self.assertTrue(all(n <= f.CHUNK for n in embedded))
        self.assertEqual(len(out), -(-secs * f.SR // f.CHUNK))

    def test_shards_resume_only_when_made_from_the_same_jobs(self):
        import json as j
        import numpy as np
        f = self.f
        jobs = [('a.flac', 'train', False, 0), ('b.flac', 'train', True, 1)]
        path = self.tmp / '0000.npy'
        np.save(path, np.zeros((40, 96), np.float16))
        path.with_suffix('.json').write_text(j.dumps({'jobs': [list(x) for x in jobs], 'frames': 40}))
        self.assertTrue(f.shard_done(path, jobs))
        self.assertFalse(f.shard_done(path, jobs[:1]))
        path.with_suffix('.json').write_text(j.dumps({'jobs': [list(x) for x in jobs], 'frames': 41}))
        self.assertFalse(f.shard_done(path, jobs))
        self.assertFalse(f.shard_done(self.tmp / 'missing.npy', jobs))



@unittest.skipUnless(_have_pipeline_deps(), 'needs the pipeline virtualenv (numpy, scipy, soundfile)')
class EvaluateTests(unittest.TestCase):
    """Regression: openWakeWord's reset() fills its buffer from unseeded random noise, so
    held-out scores (and the false-accept counts) changed from one evaluation to the next."""

    def test_scores_do_not_depend_on_global_random_state(self):
        import numpy as np
        sys.path.insert(0, str(HERE / 'pipeline'))
        import evaluate

        class FakeModel:
            def reset(self):
                self.noise = float(np.random.random())

            def predict(self, frame):
                return {'hey_fixture': self.noise}

        x = np.zeros(1280 * 3, np.float32)
        with unittest.mock.patch.object(evaluate, 'oww', lambda models: fake):
            fake = FakeModel()
            np.random.seed(1)
            first = evaluate.stream_scores(x, ['hey_fixture'], 'a.flac')
            np.random.seed(2)
            again = evaluate.stream_scores(x, ['hey_fixture'], 'a.flac')
            other = evaluate.stream_scores(x, ['hey_fixture'], 'b.flac')
        self.assertEqual(first['hey_fixture'].tolist(), again['hey_fixture'].tolist())
        self.assertNotEqual(first['hey_fixture'].tolist(), other['hey_fixture'].tolist())

if __name__ == '__main__':
    unittest.main()
