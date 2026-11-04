#!/usr/bin/env python3
"""Offline CLI contract tests. AWS, Docker and sleep are local stateful stubs."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

DEPLOY = Path(__file__).resolve().parents[1]

STUB = r'''#!/usr/bin/env python3
import json, os, sys, time
from pathlib import Path
p=Path(os.environ['MOCK_ROOT']); args=sys.argv[1:]; tool=Path(sys.argv[0]).name
with (p/'calls.jsonl').open('a') as f: f.write(json.dumps([tool]+args)+'\n')
s=json.loads((p/'state.json').read_text()); fail=os.environ.get('MOCK_FAIL','')
def save(): (p/'state.json').write_text(json.dumps(s))
def die(msg): print(msg,file=sys.stderr);sys.exit(2)
def val(flag,default=''): return args[args.index(flag)+1] if flag in args else default
if tool=='sleep': time.sleep(.02);sys.exit(0)
if tool=='docker':
 if fail=='image': die('image missing')
 sys.exit(0)
cmd=' '.join(args[:2]); query=val('--query')
if cmd=='cloudformation describe-stacks':
 if query=='Stacks[0].StackStatus':
  if fail=='stack-auth': die('AccessDenied')
  if not s['created']: die('Stack does not exist')
  print('UPDATE_COMPLETE')
 elif 'OutputKey==' in query:
  values={'ClusterName':'cluster','ServiceName':'gateway','AlertTopicArn':'arn:topic','GatewaySecurityGroupId':'sg-test'}
  print(next((v for k,v in values.items() if k in query),'output'))
 elif 'MaintenanceWindow' in query: print('true' if s['window'] else 'false')
 elif 'MinTasks' in query: print(json.dumps([['MinTasks','2'],['MaxTasks','24']]))
 else: print('{}')
elif cmd=='application-autoscaling describe-scalable-targets':
 print(json.dumps({'ScalableTargets':[{'MinCapacity':s['min'],'MaxCapacity':s['max'],'SuspendedState':s['flags']}]}))
elif cmd=='application-autoscaling register-scalable-target':
 if fail=='suspend': die('suspend denied')
 s['min']=int(val('--min-capacity'));s['max']=int(val('--max-capacity'))
 s['flags']={k:v=='true' for k,v in (x.split('=') for x in val('--suspended-state').split(','))}
 # Real API clamps desired into min/max even when scaling is suspended.
 s['desired']=max(s['min'],min(s['max'],s['desired']));save()
elif cmd=='ecs describe-services':
 if fail=='preflight': die('describe denied')
 n=s['desired']; td=s['td']; rollout='FAILED' if fail=='healthy' and td=='td:new' and n else 'COMPLETED'
 if td=='td:new' and n and rollout=='COMPLETED': s['healthy_checked']=True;save()
 print(json.dumps({'services':[{'status':'ACTIVE','desiredCount':n,'runningCount':n,'pendingCount':0,'taskDefinition':td,
 'deployments':[{'taskDefinition':td,'rolloutState':rollout}],
 'loadBalancers':[{'containerName':'gateway','containerPort':3001,'targetGroupArn':g} for g in ['tg:public','tg:internal']]}],'failures':[]}))
elif cmd=='ecs update-service':
 n=int(val('--desired-count'))
 if n and fail=='start': die('start denied')
 if not n:
  if fail=='stop': die('stop denied')
  s['old_live']=s['td']=='td:old'
 else:
  assert val('--task-definition')=='td:new', 'target task definition not pinned on start'
  assert s['td']=='td:new' and not s['old_live'], 'new task started while old process lives'
 s['desired']=n;save()
elif cmd=='ecs list-tasks':
 if val('--desired-status')=='RUNNING' and s['td']=='td:new': print(' '.join('task:new-'+str(i) for i in range(s['desired'])))
 else: print('task:old' if s['old_live'] and val('--desired-status')=='STOPPED' else '')
elif cmd=='ecs describe-tasks':
 if val('--tasks').startswith('task:new-'):
  assert query=='tasks[0].[taskDefinitionArn,lastStatus,containerInstanceArn]', 'incorrect ECS Task query'
  task={'taskDefinitionArn':'td:new','lastStatus':'RUNNING','containerInstanceArn':val('--tasks').replace('task:','ci:')}
  fields=query.removeprefix('tasks[0].[').removesuffix(']').split(',')
  print(json.dumps([task.get(k) for k in fields]))
 else:
  s['task_checks']+=1
  live=fail=='drain' or s['task_checks']==1
  s['old_live']=live;save();print('STOPPING' if live else 'STOPPED')
elif cmd=='ecs describe-container-instances': print(val('--container-instances').replace('ci:','i-'))
elif cmd=='elbv2 describe-target-health':
 group=val('--target-group-arn');checked=s.setdefault('health_checks',{})
 checked[group]=checked.get(group,0)+1;save()
 state='unhealthy' if fail=='alb-unhealthy' else 'initial' if checked[group]==1 else 'healthy'
 targets=[{'Target':{'Id':'i-new-'+str(i),'Port':3001},'TargetHealth':{'State':state}} for i in range(s['desired'])]
 # Enough healthy unrelated instances must not satisfy the real task check.
 targets += [{'Target':{'Id':'i-unrelated-'+str(i),'Port':3001},'TargetHealth':{'State':'healthy'}} for i in range(s['desired'])]
 print(json.dumps({'TargetHealthDescriptions':targets}))
elif cmd=='ecs describe-task-definition':
 print('wrong-image' if fail=='task-image' else 'docker.echojoy.cn/echojoy-gateway/ai-gateway:abc1234')
elif cmd=='cloudformation describe-stack-resource': print('td:new')
elif cmd=='cloudformation deploy':
 params=dict(x.split('=',1) for x in args if '=' in x and not x.startswith('--'))
 s['params'].append(params);save()
 if '--no-execute-changeset' in args: sys.exit(0)
 mode=params.get('DeploymentMode'); window=params.get('MaintenanceWindow')=='true'
 if mode=='maintenance':
  assert '--disable-rollback' in args, 'CFN could undo suspension and restart old code'
  if window:
   assert s['desired']==0 or not s['created'], 'old service was not stopped'
   assert not s['old_live'], 'old task process still alive'
   s.update(created=True,window=True,desired=0,min=0,td='td:new');save()
   if fail=='deploy': die('stack update failed')
  else:
   assert s['healthy_checked'] and all(s.get('health_checks',{}).get(g,0)>=2 for g in ['tg:public','tg:internal']), 'scaling resumed before ALBs became healthy'
   if fail=='close': die('close window failed')
   s['window']=False;s['min']=int(params.get('MinTasks',2));s['max']=int(params.get('MaxTasks',24))
   names={'DynamicScalingInSuspended':'SuspendScaleIn','DynamicScalingOutSuspended':'SuspendScaleOut','ScheduledScalingSuspended':'SuspendScheduledScaling'}
   s['flags']={k:params.get(v,'false')=='true' for k,v in names.items()};save()
 else:
  assert '--disable-rollback' not in args
  if mode=='rolling': s['td']='td:new'
  save()
elif cmd=='cloudformation list-change-sets': print('preview')
elif cmd=='cloudformation describe-change-set': print('preview only')
elif cmd in ['cloudformation delete-stack','cloudformation wait','cloudformation update-termination-protection']: pass
else: die('unexpected AWS command: '+cmd)
'''


class DeployTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='aws-deploy-test-')
        self.root = Path(self.tmp.name)
        self.deploy = self.root / 'deploy'
        shutil.copytree(DEPLOY, self.deploy, ignore=shutil.ignore_patterns('.backup', '__pycache__'))
        bin_dir = self.root / 'bin'
        bin_dir.mkdir()
        for name in ['aws', 'docker', 'sleep']:
            path = bin_dir / name
            path.write_text(STUB)
            path.chmod(0o755)
        self.env = dict(os.environ, PATH=str(bin_dir)+os.pathsep+os.environ['PATH'], MOCK_ROOT=str(self.root),
                        GATEWAY_DEPLOY_TIMEOUT_SECONDS='10')
        self.env.pop('CLIENT_IP_HEADER', None)
        self.env.pop('TRUSTED_PROXIES', None)
        self.initial = {'created': True, 'window': False, 'desired': 3, 'td': 'td:old', 'old_live': False,
                        'min': 2, 'max': 12, 'task_checks': 0, 'healthy_checked': False, 'params': [],
                        'flags': {'DynamicScalingInSuspended': True, 'DynamicScalingOutSuspended': False, 'ScheduledScalingSuspended': True}}
        self.write_state(self.initial)

    def tearDown(self):
        self.tmp.cleanup()

    def write_state(self, state):
        (self.root / 'state.json').write_text(json.dumps(state))

    def run_deploy(self, *args, failure='', ok=True):
        self.env['MOCK_FAIL'] = failure
        p = subprocess.run(['/bin/bash', str(self.deploy/'scripts/deploy.sh'), *args], env=self.env,
                           capture_output=True, text=True, timeout=25)
        if ok:
            self.assertEqual(p.returncode, 0, p.stdout+p.stderr)
        else:
            self.assertNotEqual(p.returncode, 0, p.stdout+p.stderr)
        calls = self.root/'calls.jsonl'
        return json.loads((self.root/'state.json').read_text()), [json.loads(x) for x in calls.read_text().splitlines()] if calls.exists() else []

    def test_default_maintenance_preserves_capacity_and_suspension(self):
        s, calls = self.run_deploy('gateway', '--image-tag', 'abc1234')
        self.assertEqual(s['desired'], 3)
        self.assertEqual((s['min'], s['max'], s['flags']), (2, 12, self.initial['flags']))
        self.assertFalse(s['window'])
        self.assertGreaterEqual(s['task_checks'], 2)
        self.assertTrue(all(s['health_checks'][g] >= 2 for g in ['tg:public', 'tg:internal']))
        self.assertEqual([x['MaintenanceWindow'] for x in s['params']], ['true', 'false'])
        self.assertEqual(s['params'][0]['ClientIPHeader'], 'X-Forwarded-For')
        self.assertEqual(s['params'][0]['TrustedProxies'], '')
        self.assertTrue(any(x[1:3]==['ecs','describe-tasks'] for x in calls))

    def test_template_wires_zero_capacity_and_mode_specific_rollback(self):
        template = (self.deploy/'gateway.yaml').read_text()
        for required in [
            "DesiredCount: !If [InMaintenance, 0, !Ref 'AWS::NoValue']",
            'MinCapacity: !If [InMaintenance, 0, !Ref MinTasks]',
            'Rollback: !If [RollingDeployment, true, false]',
            'DynamicScalingInSuspended: !If [InMaintenance, true,',
            'DynamicScalingOutSuspended: !If [InMaintenance, true,',
            'ScheduledScalingSuspended: !If [InMaintenance, true,',
            '{Name: CLIENT_IP_HEADER, Value: !Ref ClientIPHeader}',
            '{Name: TRUSTED_PROXIES, Value: !Ref TrustedProxies}',
        ]:
            self.assertIn(required, template)

    def test_explicit_empty_header_is_preserved(self):
        self.env['CLIENT_IP_HEADER'] = ''
        s, _ = self.run_deploy('gateway', '--image-tag', 'abc1234', '--mode', 'rolling')
        self.assertEqual(s['params'][0]['ClientIPHeader'], '')

    def test_custom_header_and_comma_separated_trusted_proxies(self):
        self.env.update(CLIENT_IP_HEADER='CF-Connecting-IP', TRUSTED_PROXIES='192.0.2.0/24,2001:db8::/32')
        s, _ = self.run_deploy('gateway', '--image-tag', 'abc1234', '--mode', 'rolling')
        self.assertEqual(s['params'][0]['TrustedProxies'], '192.0.2.0/24,2001:db8::/32')
        self.assertEqual(s['params'][0]['ClientIPHeader'], 'CF-Connecting-IP')

    def test_explicit_rolling_does_not_stop_or_suspend(self):
        s, calls = self.run_deploy('gateway', '--image-tag', 'abc1234', '--mode', 'rolling')
        self.assertEqual(len(s['params']), 1)
        self.assertEqual(s['desired'], 3)
        self.assertEqual(s['params'][0]['SuspendScaleIn'], 'true')
        self.assertEqual(s['params'][0]['SuspendScaleOut'], 'false')
        self.assertFalse(any(x[1:3] in [['ecs','update-service'], ['application-autoscaling','register-scalable-target']] for x in calls))

    def test_preview_never_stops_tasks(self):
        s, calls = self.run_deploy('gateway', '--image-tag', 'abc1234', '--dry-run')
        self.assertEqual(s['desired'], 3)
        self.assertFalse(any(x[0]=='docker' or x[1:3] in [['ecs','update-service'], ['application-autoscaling','register-scalable-target']] for x in calls))
        self.assertTrue(any('--no-execute-changeset' in x for x in calls))

    def test_network_and_data_keep_existing_deploy_path(self):
        for target in ['network', 'data']:
            with self.subTest(target=target):
                (self.root/'calls.jsonl').unlink(missing_ok=True)
                s, calls = self.run_deploy(target)
                self.assertEqual(s['desired'], 3)
                self.assertFalse(any(x[0]=='docker' or x[1] in ['ecs', 'application-autoscaling'] for x in calls))

    def test_first_deployment_starts_only_after_stack_creation(self):
        self.initial.update(created=False, desired=0)
        self.write_state(self.initial)
        s, _ = self.run_deploy('gateway', '--image-tag', 'abc1234')
        self.assertEqual(s['desired'], 2)
        self.assertEqual(s['min'], 2)
        self.assertTrue(s['healthy_checked'])

    def test_failed_updates_keep_zero_capacity_and_do_not_rollback(self):
        for stage in ['deploy', 'start', 'healthy', 'task-image', 'close']:
            with self.subTest(stage=stage):
                self.write_state(self.initial)
                s, _ = self.run_deploy('gateway', '--image-tag', 'abc1234', failure=stage, ok=False)
                self.assertEqual(s['desired'], 0)
                self.assertEqual(s['min'], 0)
                self.assertTrue(all(s['flags'].values()))
                if stage != 'close':
                    self.assertFalse(any(p.get('MaintenanceWindow')=='false' for p in s['params']))

    def test_running_tasks_without_healthy_alb_targets_do_not_close_window(self):
        self.env['GATEWAY_DEPLOY_TIMEOUT_SECONDS'] = '2'
        s, _ = self.run_deploy('gateway', '--image-tag', 'abc1234', failure='alb-unhealthy', ok=False)
        self.assertTrue(s['healthy_checked'])
        self.assertEqual(s['desired'], 0)
        self.assertEqual([p['MaintenanceWindow'] for p in s['params']], ['true'])

    def test_failed_aws_mutations_do_not_reach_stack_update(self):
        for failure in ['suspend', 'stop']:
            with self.subTest(failure=failure):
                self.write_state(self.initial)
                s, _ = self.run_deploy('gateway', '--image-tag', 'abc1234', failure=failure, ok=False)
                self.assertEqual(s['params'], [])

    def test_old_task_still_stopping_prevents_stack_update(self):
        self.env['GATEWAY_DEPLOY_TIMEOUT_SECONDS'] = '1'
        s, _ = self.run_deploy('gateway', '--image-tag', 'abc1234', failure='drain', ok=False)
        self.assertEqual(s['params'], [])
        self.assertEqual(s['desired'], 0)

    def test_read_errors_and_missing_image_never_mutate_service(self):
        for failure in ['preflight', 'image', 'stack-auth']:
            with self.subTest(failure=failure):
                (self.root/'calls.jsonl').unlink(missing_ok=True)
                _, calls = self.run_deploy('gateway', '--image-tag', 'abc1234', failure=failure, ok=False)
                self.assertFalse(any(x[1:3] in [['ecs','update-service'], ['application-autoscaling','register-scalable-target'], ['cloudformation','deploy']] for x in calls))

    def test_invalid_options_fail_before_aws(self):
        for args in [('gateway','--mode','unsafe'), ('gateway','--image-tag'), ('gateway','--image-tag','bad tag'), ('data','--mode','maintenance')]:
            with self.subTest(args=args):
                _, calls = self.run_deploy(*args, ok=False)
                self.assertEqual(calls, [])

    def test_stopped_service_and_open_window_require_manual_recovery(self):
        self.initial.update(desired=0)
        self.write_state(self.initial)
        s, _ = self.run_deploy('gateway', '--image-tag', 'abc1234', ok=False)
        self.assertEqual(s['params'], [])
        self.initial.update(window=True)
        self.write_state(self.initial)
        s, _ = self.run_deploy('gateway', '--image-tag', 'abc1234', '--mode', 'rolling', ok=False)
        self.assertEqual(s['params'], [])


if __name__ == '__main__':
    unittest.main(verbosity=2)
