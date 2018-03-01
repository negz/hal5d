package cert

import (
	"fmt"
	"hash/fnv"
	"io"
	"path/filepath"
	"strings"

	"github.com/negz/hal5d/internal/kubernetes"

	"github.com/pkg/errors"
	"github.com/spf13/afero"
	"go.uber.org/zap"
	"k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
)

const (
	certPairSuffix    = ".pem"
	certPairSeparator = "-"
	certPairMode      = 0600
)

type certPair struct {
	Namespace   string
	IngressName string
	SecretName  string
}

func newCertPair(filename string) (certPair, error) {
	if !strings.HasSuffix(filename, certPairSuffix) {
		return certPair{}, errors.Errorf("filename %s does not end with expected suffix %s", filename, certPairSuffix)
	}
	parts := strings.Split(strings.TrimSuffix(filename, certPairSuffix), certPairSeparator)
	if len(parts) != 3 {
		return certPair{}, errors.Errorf("filename %s does not match expected namespace-ingressname-secretname.pem pattern", filename)
	}
	return certPair{Namespace: parts[0], IngressName: parts[1], SecretName: parts[2]}, nil
}

func (c certPair) Filename() string {
	return fmt.Sprintf("%s-%s-%s.pem", c.Namespace, c.IngressName, c.SecretName)
}

type certData struct {
	certPair
	Cert []byte
	Key  []byte
}

type metadata struct {
	Namespace string
	Name      string
}

type secretRefs map[metadata]map[string]bool

func (r secretRefs) Add(namespace, ingressName, secretName string) {
	m := metadata{Namespace: namespace, Name: secretName}
	if _, ok := r[m]; !ok {
		r[m] = make(map[string]bool)
	}
	r[m][ingressName] = true
}

func (r secretRefs) Delete(namespace, ingressName, secretName string) {
	m := metadata{Namespace: namespace, Name: secretName}
	delete(r[m], ingressName)
}

func (r secretRefs) Get(namespace, secretName string) map[string]bool {
	m := metadata{Namespace: namespace, Name: secretName}
	return r[m]
}

// A Validator determines whether cert pairs are valid.
type Validator interface {
	Validate() error
}

// A Subscriber is notified synchronously every time the cert pairs change.
type Subscriber interface {
	// Changed is called every time the managed certificates change.
	Changed()
}

type optimisticValidator struct{}

func (v *optimisticValidator) Validate() error {
	return nil
}

// A Manager persists ingress TLS cert pairs to disk. Manager implements
// cache.ResourceEventHandler in order to consume notifications about
type Manager struct {
	log *zap.Logger
	fs  afero.Fs

	tlsDir      string
	v           Validator
	secretStore kubernetes.SecretStore
	secretRefs  secretRefs
	subscribers []Subscriber
}

// A ManagerOption can be used to configure new certificate managers.
type ManagerOption func(*Manager) error

// WithLogger configures a certificate manager's logger.
func WithLogger(l *zap.Logger) ManagerOption {
	return func(m *Manager) error {
		m.log = l
		return nil
	}
}

// WithFilesystem configures a certificate manager's filesystem implementation.
func WithFilesystem(fs afero.Fs) ManagerOption {
	return func(m *Manager) error {
		m.fs = fs
		return nil
	}
}

// WithValidator configures a certificate manager's validator. The validator
// will be called to test any new cert pairs before they are committed.
func WithValidator(v Validator) ManagerOption {
	return func(m *Manager) error {
		m.v = v
		return nil
	}
}

// WithSubscriber registers a subscriber to a certificate manager. Each
// subscriber will be called every time the managed cert pairs change.
func WithSubscriber(s Subscriber) ManagerOption {
	return func(m *Manager) error {
		m.subscribers = append(m.subscribers, s)
		return nil
	}
}

// NewManager creates a new certificate manager.
func NewManager(dir string, s kubernetes.SecretStore, o ...ManagerOption) (*Manager, error) {
	m := &Manager{
		log:         zap.NewNop(),
		fs:          afero.NewOsFs(),
		tlsDir:      dir,
		v:           &optimisticValidator{},
		secretStore: s,
		secretRefs:  make(map[metadata]map[string]bool),
		subscribers: make([]Subscriber, 0),
	}
	for _, mo := range o {
		if err := mo(m); err != nil {
			return nil, errors.Wrap(err, "cannot apply manager option")
		}
	}
	return m, nil
}

// OnAdd handles notifications of new ingress or secret resources.
func (m *Manager) OnAdd(obj interface{}) {
	switch obj := obj.(type) {
	case *v1beta1.Ingress:
		if changed := m.upsertIngress(obj); changed {
			m.notifySubscribers()
		}
	case *v1.Secret:
		if changed := m.upsertSecret(obj); changed {
			m.notifySubscribers()
		}
	}
}

// OnUpdate handles notifications of updated ingress or secret resources.
func (m *Manager) OnUpdate(_, newObj interface{}) {
	m.OnAdd(newObj)
}

// OnDelete handles notifications of deleted ingress or secret resources.
func (m *Manager) OnDelete(obj interface{}) {
	switch obj := obj.(type) {
	case *v1beta1.Ingress:
		if changed := m.deleteIngress(obj); changed {
			m.notifySubscribers()
		}
	case *v1.Secret:
		if changed := m.deleteSecret(obj); changed {
			m.notifySubscribers()
		}
	}
}

func (m *Manager) upsertIngress(i *v1beta1.Ingress) bool { // nolint:gocyclo
	log := m.log.With(
		zap.String("namespace", i.GetNamespace()),
		zap.String("ingress name", i.GetName()))

	existing, err := m.existing(i.GetNamespace(), i.GetName())
	if err != nil {
		log.Error("cannot get existing cert pairs - stale cert pairs will not be reaped")
	}

	changed := false
	keep := make(map[certPair]bool)
	for _, tls := range i.Spec.TLS {
		m.secretRefs.Add(i.GetNamespace(), i.GetName(), tls.SecretName)
		s, err := m.secretStore.Get(i.GetNamespace(), tls.SecretName)
		if err != nil {
			log.Info("cannot get TLS secret",
				zap.String("secret name", tls.SecretName),
				zap.Error(err))
			continue
		}

		cert, ok := s.Data[v1.TLSCertKey]
		if !ok {
			log.Info("missing certificate",
				zap.String("secret name", s.GetName()),
				zap.String("secret key", v1.TLSCertKey))
			continue
		}
		key, ok := s.Data[v1.TLSPrivateKeyKey]
		if !ok {
			log.Info("missing private key",
				zap.String("secret name", s.GetName()),
				zap.String("secret key", v1.TLSPrivateKeyKey))
			continue
		}

		cp := certPair{Namespace: i.GetNamespace(), IngressName: i.GetName(), SecretName: s.GetName()}
		cd := certData{certPair: cp, Cert: cert, Key: key}
		if existing[cp] && !m.changed(cd) {
			log.Debug("cert pair unchanged", zap.String("secret name", tls.SecretName))
			keep[cp] = true
			continue
		}
		if err := m.write(cd); err != nil {
			log.Error("cannot write cert pair",
				zap.String("secret name", tls.SecretName),
				zap.Error(err))
			continue
		}
		keep[cp] = true
		changed = true
	}

	for cp := range existing {
		if keep[cp] {
			continue
		}
		path := filepath.Join(m.tlsDir, cp.Filename())
		if err := m.fs.Remove(path); err != nil {
			log.Error("cannot remove stale cert pair",
				zap.String("secret name", cp.SecretName),
				zap.Error(err))
			continue
		}
		m.secretRefs.Delete(i.GetNamespace(), i.GetName(), cp.SecretName)
		changed = true
	}

	return changed
}

func (m *Manager) changed(c certData) bool {
	f, err := m.fs.Open(filepath.Join(m.tlsDir, c.Filename()))
	if err != nil {
		return true
	}
	defer f.Close()

	existing := fnv.New32a()
	if _, err := io.Copy(existing, f); err != nil {
		return true
	}

	proposed := fnv.New32a()
	if _, err := proposed.Write(c.Cert); err != nil {
		return true
	}
	if _, err := proposed.Write(c.Key); err != nil {
		return true
	}

	return proposed.Sum32() != existing.Sum32()
}

func (m *Manager) write(c certData) error {
	f, err := afero.TempFile(m.fs, m.tlsDir, c.Filename())
	if err != nil {
		return errors.Wrapf(err, "cannot create temp file in %v", m.tlsDir)
	}
	defer f.Close()
	defer m.fs.Remove(f.Name())

	if _, err := f.Write(c.Cert); err != nil {
		return errors.Wrapf(err, "cannot write cert data to %v", f.Name())
	}
	if _, err := f.Write(c.Key); err != nil {
		return errors.Wrapf(err, "cannot write key data to %v", f.Name())
	}
	if err := f.Sync(); err != nil {
		return errors.Wrapf(err, "cannot fsync %v", f.Name())
	}
	if err := f.Close(); err != nil {
		return errors.Wrapf(err, "cannot close %v", f.Name())
	}
	if err := m.fs.Chmod(f.Name(), certPairMode); err != nil {
		return errors.Wrapf(err, "cannot chmod %v to %d", f.Name(), certPairMode)
	}
	// This assumes the validate function treats the temp file as it would any
	// other file in the TLS directory.
	if err := m.v.Validate(); err != nil {
		return errors.Wrapf(err, "writing certificate pair would result in invalid configuration")
	}
	path := filepath.Join(m.tlsDir, c.Filename())
	return errors.Wrapf(m.fs.Rename(f.Name(), path), "cannot move %v to %v", f.Name(), path)
}

func (m *Manager) upsertSecret(s *v1.Secret) bool {
	log := m.log.With(
		zap.String("namespace", s.GetNamespace()),
		zap.String("secret name", s.GetName()))

	changed := false
	for ingressName := range m.secretRefs.Get(s.GetNamespace(), s.GetName()) {
		cert, ok := s.Data[v1.TLSCertKey]
		if !ok {
			m.log.Info("missing TLS certificate",
				zap.String("ingress name", ingressName),
				zap.String("secret key", v1.TLSCertKey))
			continue
		}
		key, ok := s.Data[v1.TLSPrivateKeyKey]
		if !ok {
			m.log.Info("missing TLS private key",
				zap.String("ingress name", ingressName),
				zap.String("secret key", v1.TLSPrivateKeyKey))
			continue
		}

		cp := certPair{Namespace: s.GetNamespace(), IngressName: ingressName, SecretName: s.GetName()}
		cd := certData{certPair: cp, Cert: cert, Key: key}
		if !m.changed(cd) {
			log.Debug("cert pair unchanged", zap.String("ingress name", ingressName))
			continue
		}
		if err := m.write(cd); err != nil {
			log.Error("cannot write cert pair",
				zap.String("ingress name", ingressName),
				zap.Error(err))
			continue
		}
		changed = true
	}

	return changed
}

func (m *Manager) deleteIngress(i *v1beta1.Ingress) bool {
	log := m.log.With(
		zap.String("namespace", i.GetNamespace()),
		zap.String("ingress name", i.GetName()))

	changed := false
	existing, err := m.existing(i.GetNamespace(), i.GetName())
	if err != nil {
		log.Error("cannot get existing cert pairs - stale cert pairs will not be reaped")
	}
	for cp := range existing {
		path := filepath.Join(m.tlsDir, cp.Filename())
		if err := m.fs.Remove(path); err != nil {
			log.Error("cannot remove stale cert pair",
				zap.String("secret name", cp.SecretName),
				zap.Error(err))
			continue
		}
		m.secretRefs.Delete(i.GetNamespace(), i.GetName(), cp.SecretName)
		changed = true
	}

	return changed
}

func (m *Manager) deleteSecret(s *v1.Secret) bool {
	log := m.log.With(
		zap.String("namespace", s.GetNamespace()),
		zap.String("secret name", s.GetName()))

	changed := false
	for ingressName := range m.secretRefs.Get(s.GetNamespace(), s.GetName()) {
		cp := certPair{Namespace: s.GetNamespace(), IngressName: ingressName, SecretName: s.GetName()}
		path := filepath.Join(m.tlsDir, cp.Filename())
		if err := m.fs.Remove(path); err != nil {
			log.Error("cannot remove stale TLS certpair",
				zap.String("secret name", cp.SecretName),
				zap.Error(err))
			continue
		}
		m.secretRefs.Delete(s.GetNamespace(), ingressName, s.GetName())
		changed = true
	}

	return changed
}

func (m *Manager) existing(namespace, ingressName string) (map[certPair]bool, error) {
	fi, err := afero.ReadDir(m.fs, m.tlsDir)
	if err != nil {
		return nil, errors.Wrap(err, "cannot list TLS cert pairs")
	}

	pairs := make(map[certPair]bool)
	for _, f := range fi {
		c, err := newCertPair(f.Name())
		if err != nil {
			m.log.Debug("unexpected file in TLS dir",
				zap.String("filename", f.Name()),
				zap.String("tlsDir", m.tlsDir))
			continue
		}
		if c.Namespace != namespace {
			continue
		}
		if c.IngressName != ingressName {
			continue
		}
		pairs[c] = true
	}
	return pairs, nil
}

func (m *Manager) notifySubscribers() {
	for _, s := range m.subscribers {
		s.Changed()
	}
}