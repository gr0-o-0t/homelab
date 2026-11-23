package db

import (
	"context"
	"strings"
	"testing"

	"github.com/groot/homelab/internal/config"
)

const rx = "docker exec homelab-rabbitmq rabbitmqctl "

// rabbitProvisioner answers `list_users` with users, one name per line.
func rabbitProvisioner(users ...string) (*Provisioner, *fakeCommander, *fakeSM) {
	fc := &fakeCommander{outputFn: func(key string) ([]byte, error, bool) {
		if strings.HasPrefix(key, rx+"-q list_users") {
			var out string
			for _, u := range users {
				out += u + "\t[]\n"
			}
			return []byte(out), nil, true
		}
		return nil, nil, false
	}}
	sm := &fakeSM{store: map[string]string{}}
	return &Provisioner{ContainerFor: fixedNames, RC: fc, SM: sm}, fc, sm
}

// rabbitCalls lists every rabbitmqctl invocation in order, with any stdin.
func rabbitCalls(fc *fakeCommander) string {
	var out []string
	for _, c := range append(fc.outputCalls, fc.runCalls...) {
		line := strings.TrimPrefix(c.Name+" "+strings.Join(c.Args, " "), "docker exec -i homelab-rabbitmq rabbitmqctl ")
		line = strings.TrimPrefix(line, rx)
		if c.Stdin != "" {
			line += " <<" + c.Stdin
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestProvisionRabbitMQ_FirstRun(t *testing.T) {
	p, fc, sm := rabbitProvisioner("admin")
	if err := p.Provision(context.Background(), config.DBAMQP, "celery", config.ServiceDBDecl{}); err != nil {
		t.Fatal(err)
	}
	pw := sm.store["celery/"+config.AMQPPasswordKey]
	if len(pw) < 32 {
		t.Fatalf("password not stored under the service namespace: %v", sm.store)
	}
	got := rabbitCalls(fc)
	for _, want := range []string{
		"-q add_vhost celery",
		"add_user celery <<" + pw,
		"-q set_permissions -p celery celery .* .* .*",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, c := range append(fc.outputCalls, fc.runCalls...) {
		if strings.Contains(strings.Join(c.Args, " "), pw) {
			t.Fatalf("password in argv: %v", c.Args)
		}
	}
}

// A repeated provision re-syncs the existing user's password with the vault
// instead of failing on add_user.
func TestProvisionRabbitMQ_ExistingUserGetsPasswordResynced(t *testing.T) {
	p, fc, sm := rabbitProvisioner("admin", "celery")
	sm.store["celery/"+config.AMQPPasswordKey] = "storedpw"
	if err := p.Provision(context.Background(), config.DBAMQP, "celery", config.ServiceDBDecl{}); err != nil {
		t.Fatal(err)
	}
	got := rabbitCalls(fc)
	if strings.Contains(got, "add_user") || !strings.Contains(got, "change_password celery <<storedpw") {
		t.Fatalf("want change_password with the stored password, got:\n%s", got)
	}
}

func TestDeprovisionRabbitMQ_DeletesUserKeepsVhost(t *testing.T) {
	p, fc, sm := rabbitProvisioner("admin", "celery")
	sm.store["celery/"+config.AMQPPasswordKey] = "storedpw"
	if err := p.Deprovision(context.Background(), config.DBAMQP, "celery", config.ServiceDBDecl{}); err != nil {
		t.Fatal(err)
	}
	got := rabbitCalls(fc)
	if !strings.Contains(got, "-q delete_user celery") || strings.Contains(got, "delete_vhost") {
		t.Fatalf("want the user deleted and the vhost kept, got:\n%s", got)
	}
	if _, ok := sm.store["celery/"+config.AMQPPasswordKey]; ok {
		t.Fatal("password left in the vault")
	}
}

// The management admin is RabbitMQAdminUser; a service of that name would
// take over its password.
func TestProvisionRabbitMQ_RefusesAdminName(t *testing.T) {
	p, _, _ := rabbitProvisioner("admin")
	if err := p.Provision(context.Background(), config.DBAMQP, config.RabbitMQAdminUser, config.ServiceDBDecl{}); err == nil {
		t.Fatal("provisioning a service named after the admin user must fail")
	}
}
