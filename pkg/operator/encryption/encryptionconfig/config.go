package encryptionconfig

import (
	"encoding/base64"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apiserverconfigv1 "k8s.io/apiserver/pkg/apis/config/v1"
	"k8s.io/klog"

	"github.com/PavloVaida/library-go/pkg/operator/encryption/crypto"
	"github.com/PavloVaida/library-go/pkg/operator/encryption/secrets"
	"github.com/PavloVaida/library-go/pkg/operator/encryption/state"
)

var (
	emptyStaticIdentityKey = base64.StdEncoding.EncodeToString(crypto.NewIdentityKey())
)

// FromEncryptionState converts state to config.
func FromEncryptionState(encryptionState map[schema.GroupResource]state.GroupResourceState) *apiserverconfigv1.EncryptionConfiguration {
	resourceConfigs := make([]apiserverconfigv1.ResourceConfiguration, 0, len(encryptionState))

	for gr, grKeys := range encryptionState {
		resourceConfigs = append(resourceConfigs, apiserverconfigv1.ResourceConfiguration{
			Resources: []string{gr.String()}, // we are forced to lose data here because this API is broken
			Providers: stateToProviders(grKeys),
		})
	}

	// make sure our output is stable
	sort.Slice(resourceConfigs, func(i, j int) bool {
		return resourceConfigs[i].Resources[0] < resourceConfigs[j].Resources[0] // each resource has its own keys
	})

	return &apiserverconfigv1.EncryptionConfiguration{Resources: resourceConfigs}
}

// ToEncryptionState converts config to state.
// Read keys contain a potential write key. Read keys are sorted, recent first.
//
// It assumes:
// - the first provider provides the write key
// - the structure of the encryptionConfig matches the output generated by FromEncryptionState:
//   - one resource per provider
//   - one key per provider
// - each resource has a distinct configuration with zero or more key based providers and the identity provider.
// - the last providers might be of type aesgcm. Then it carries the names of identity keys, recent first.
//   We never use aesgcm as a real key because it is unsafe.
func ToEncryptionState(encryptionConfig *apiserverconfigv1.EncryptionConfiguration, keySecrets []*corev1.Secret) (map[schema.GroupResource]state.GroupResourceState, []state.KeyState) {
	backedKeys := make([]state.KeyState, 0, len(keySecrets))
	for _, s := range keySecrets {
		km, err := secrets.ToKeyState(s)
		if err != nil {
			klog.Warningf("skipping invalid secret: %v", err)
			continue
		}
		km.Backed = true
		backedKeys = append(backedKeys, km)
	}
	backedKeys = state.SortRecentFirst(backedKeys)

	if encryptionConfig == nil {
		return nil, backedKeys
	}

	out := map[schema.GroupResource]state.GroupResourceState{}
	for _, resourceConfig := range encryptionConfig.Resources {
		// resources should be a single group resource
		if len(resourceConfig.Resources) != 1 {
			klog.Warningf("skipping invalid encryption config for resource %s", resourceConfig.Resources)
			continue // should never happen
		}

		grState := state.GroupResourceState{}

		for i, provider := range resourceConfig.Providers {
			var ks state.KeyState

			switch {
			case provider.AESCBC != nil && len(provider.AESCBC.Keys) == 1:
				ks = state.KeyState{
					Key:  provider.AESCBC.Keys[0],
					Mode: state.AESCBC,
				}

			case provider.Secretbox != nil && len(provider.Secretbox.Keys) == 1:
				ks = state.KeyState{
					Key:  provider.Secretbox.Keys[0],
					Mode: state.SecretBox,
				}

			case provider.Identity != nil:
				// skip fake provider. If this is write-key, wait for first aesgcm provider providing the write key.
				continue

			case provider.AESGCM != nil && len(provider.AESGCM.Keys) == 1 && provider.AESGCM.Keys[0].Secret == emptyStaticIdentityKey:
				ks = state.KeyState{
					Key:  provider.AESGCM.Keys[0],
					Mode: state.Identity,
				}

			default:
				klog.Infof("skipping invalid provider index %d for resource %s", i, resourceConfig.Resources[0])
				continue // should never happen
			}

			// enrich KeyState with values from secrets
			for _, k := range backedKeys {
				if state.EqualKeyAndEqualID(&ks, &k) {
					ks = k
					break
				}
			}

			if i == 0 || (ks.Mode == state.Identity && !grState.HasWriteKey()) {
				grState.WriteKey = ks
			}

			grState.ReadKeys = append(grState.ReadKeys, ks) // also for write key as they are also read keys
		}

		// sort read-keys, recent first
		grState.ReadKeys = state.SortRecentFirst(grState.ReadKeys)

		out[schema.ParseGroupResource(resourceConfig.Resources[0])] = grState
	}

	return out, backedKeys
}

// stateToProviders maps the write and read secrets to the equivalent read and write keys.
// it primarily handles the conversion of KeyState to the appropriate provider config.
// the identity mode is transformed into a custom aesgcm provider that simply exists to
// curry the associated null key secret through the encryption state machine.
func stateToProviders(desired state.GroupResourceState) []apiserverconfigv1.ProviderConfiguration {
	allKeys := desired.ReadKeys

	providers := make([]apiserverconfigv1.ProviderConfiguration, 0, len(allKeys)+1) // one extra for identity

	// Write key comes first. Filter it out in the tail of read keys.
	if desired.HasWriteKey() {
		allKeys = append([]state.KeyState{desired.WriteKey}, allKeys...)
		for i := 1; i < len(allKeys); i++ {
			if state.EqualKeyAndEqualID(&allKeys[i], &desired.WriteKey) {
				allKeys = append(allKeys[:i], allKeys[i+1:]...)
				break
			}
		}
	} else {
		// no write key => identity write key
		providers = append(providers, apiserverconfigv1.ProviderConfiguration{
			Identity: &apiserverconfigv1.IdentityConfiguration{},
		})
	}

	aesgcmProviders := []apiserverconfigv1.ProviderConfiguration{}
	for i, key := range allKeys {
		switch key.Mode {
		case state.AESCBC:
			providers = append(providers, apiserverconfigv1.ProviderConfiguration{
				AESCBC: &apiserverconfigv1.AESConfiguration{
					Keys: []apiserverconfigv1.Key{key.Key},
				},
			})
		case state.SecretBox:
			providers = append(providers, apiserverconfigv1.ProviderConfiguration{
				Secretbox: &apiserverconfigv1.SecretboxConfiguration{
					Keys: []apiserverconfigv1.Key{key.Key},
				},
			})
		case state.Identity:
			if i == 0 {
				providers = append(providers, apiserverconfigv1.ProviderConfiguration{
					Identity: &apiserverconfigv1.IdentityConfiguration{},
				})
			}
			aesgcmProviders = append(aesgcmProviders, apiserverconfigv1.ProviderConfiguration{
				AESGCM: &apiserverconfigv1.AESConfiguration{
					Keys: []apiserverconfigv1.Key{key.Key},
				},
			})
		default:
			// this should never happen because our input should always be valid
			klog.Infof("skipping key %s as it has invalid mode %s", key.Key.Name, key.Mode)
		}
	}

	// add fallback identity provider.
	if providers[0].Identity == nil {
		providers = append(providers, apiserverconfigv1.ProviderConfiguration{
			Identity: &apiserverconfigv1.IdentityConfiguration{},
		})
	}

	// add fake aesgm providers carrying identity names
	providers = append(providers, aesgcmProviders...)

	return providers
}
