/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/controller-runtime/pkg/webhook/conversion"
)

// This code is copied from https://github.com/kubernetes-sigs/controller-runtime/blob/896f6ded750155f9ecfdf4d8e10a26fc3fb78384/pkg/builder/webhook.go
// with one modification to get full control of the construction of patches:
// replacing CustomDefaulter with an admission.Handler.
// TODO(#3137): remove this file

// WebhookBuilder builds a Webhook.
type WebhookBuilder struct {
	apiType         runtime.Object
	mutationHandler admission.Handler
	customValidator admission.CustomValidator
	gvk             schema.GroupVersionKind
	mgr             manager.Manager
	config          *rest.Config
	recoverPanic    *bool
	logConstructor  func(base logr.Logger, req *admission.Request) logr.Logger
	err             error
}

// WebhookManagedBy returns a new webhook builder.
func WebhookManagedBy(m manager.Manager) *WebhookBuilder {
	return &WebhookBuilder{mgr: m}
}

// TODO(droot): update the GoDoc for conversion.

// For takes a runtime.Object which should be a CR.
// If the given object implements the admission.Defaulter interface, a MutatingWebhook will be wired for this type.
// If the given object implements the admission.Validator interface, a ValidatingWebhook will be wired for this type.
func (blder *WebhookBuilder) For(apiType runtime.Object) *WebhookBuilder {
	if blder.apiType != nil {
		blder.err = errors.New("For(...) should only be called once, could not assign multiple objects for webhook registration")
	}
	blder.apiType = apiType
	return blder
}

// WithMutationHandler takes an admission.CustomDefaulter interface, a MutatingWebhook will be wired for this type.
func (blder *WebhookBuilder) WithMutationHandler(h admission.Handler) *WebhookBuilder {
	blder.mutationHandler = h
	return blder
}

// WithValidator takes a admission.CustomValidator interface, a ValidatingWebhook will be wired for this type.
func (blder *WebhookBuilder) WithValidator(validator admission.CustomValidator) *WebhookBuilder {
	blder.customValidator = validator
	return blder
}

// WithLogConstructor overrides the webhook's LogConstructor.
func (blder *WebhookBuilder) WithLogConstructor(logConstructor func(base logr.Logger, req *admission.Request) logr.Logger) *WebhookBuilder {
	blder.logConstructor = logConstructor
	return blder
}

// RecoverPanic indicates whether panics caused by the webhook should be recovered.
// Defaults to true.
func (blder *WebhookBuilder) RecoverPanic(recoverPanic bool) *WebhookBuilder {
	blder.recoverPanic = &recoverPanic
	return blder
}

// Complete builds the webhook.
func (blder *WebhookBuilder) Complete() error {
	// Set the Config
	blder.loadRestConfig()

	// Configure the default LogConstructor
	blder.setLogConstructor()

	// Set the Webhook if needed
	return blder.registerWebhooks()
}

func (blder *WebhookBuilder) loadRestConfig() {
	if blder.config == nil {
		blder.config = blder.mgr.GetConfig()
	}
}

func (blder *WebhookBuilder) setLogConstructor() {
	if blder.logConstructor == nil {
		blder.logConstructor = func(base logr.Logger, req *admission.Request) logr.Logger {
			log := base.WithValues(
				"webhookGroup", blder.gvk.Group,
				"webhookKind", blder.gvk.Kind,
			)
			if req != nil {
				return log.WithValues(
					blder.gvk.Kind, klog.KRef(req.Namespace, req.Name),
					"namespace", req.Namespace, "name", req.Name,
					"resource", req.Resource, "user", req.UserInfo.Username,
					"requestID", req.UID,
				)
			}
			return log
		}
	}
}

func (blder *WebhookBuilder) registerWebhooks() error {
	typ, err := blder.getType()
	if err != nil {
		return err
	}

	blder.gvk, err = apiutil.GVKForObject(typ, blder.mgr.GetScheme())
	if err != nil {
		return err
	}

	// Register webhook(s) for type
	blder.registerDefaultingWebhook()
	blder.registerValidatingWebhook()

	err = blder.registerConversionWebhook()
	if err != nil {
		return err
	}
	return blder.err
}

// registerDefaultingWebhook registers a defaulting webhook if necessary.
func (blder *WebhookBuilder) registerDefaultingWebhook() {
	mwh := blder.getDefaultingWebhook()
	if mwh != nil {
		mwh.LogConstructor = blder.logConstructor
		path := generateMutatePath(blder.gvk)

		// Checking if the path is already registered.
		// If so, just skip it.
		if !blder.isAlreadyHandled(path) {
			log := blder.mgr.GetLogger()
			log.Info("Registering a mutating webhook",
				"GVK", blder.gvk,
				"path", path)
			blder.mgr.GetWebhookServer().Register(path, mwh)
		}
	}
}

func (blder *WebhookBuilder) getDefaultingWebhook() *admission.Webhook {
	if handler := blder.mutationHandler; handler != nil {
		w := &admission.Webhook{
			Handler: handler,
		}
		if blder.recoverPanic != nil {
			w = w.WithRecoverPanic(*blder.recoverPanic)
		}
		return w
	}
	return nil
}

// registerValidatingWebhook registers a validating webhook if necessary.
func (blder *WebhookBuilder) registerValidatingWebhook() {
	vwh := blder.getValidatingWebhook()
	if vwh != nil {
		vwh.LogConstructor = blder.logConstructor
		path := generateValidatePath(blder.gvk)

		// Checking if the path is already registered.
		// If so, just skip it.
		if !blder.isAlreadyHandled(path) {
			log := blder.mgr.GetLogger()
			log.Info("Registering a validating webhook",
				"GVK", blder.gvk,
				"path", path)
			blder.mgr.GetWebhookServer().Register(path, vwh)
		}
	}
}

func (blder *WebhookBuilder) getValidatingWebhook() *admission.Webhook {
	if validator := blder.customValidator; validator != nil {
		w := admission.WithCustomValidator(blder.mgr.GetScheme(), blder.apiType, validator)
		if blder.recoverPanic != nil {
			w = w.WithRecoverPanic(*blder.recoverPanic)
		}
		return w
	}
	return nil
}

func (blder *WebhookBuilder) registerConversionWebhook() error {
	log := blder.mgr.GetLogger()
	ok, err := conversion.IsConvertible(blder.mgr.GetScheme(), blder.apiType)
	if err != nil {
		log.Error(err, "conversion check failed", "GVK", blder.gvk)
		return err
	}
	if ok {
		if !blder.isAlreadyHandled("/convert") {
			blder.mgr.GetWebhookServer().Register("/convert", conversion.NewWebhookHandler(blder.mgr.GetScheme()))
		}
		log.Info("Conversion webhook enabled", "GVK", blder.gvk)
	}

	return nil
}

func (blder *WebhookBuilder) getType() (runtime.Object, error) {
	if blder.apiType != nil {
		return blder.apiType, nil
	}
	return nil, errors.New("For() must be called with a valid object")
}

func (blder *WebhookBuilder) isAlreadyHandled(path string) bool {
	if blder.mgr.GetWebhookServer().WebhookMux() == nil {
		return false
	}
	h, p := blder.mgr.GetWebhookServer().WebhookMux().Handler(&http.Request{URL: &url.URL{Path: path}})
	if p == path && h != nil {
		return true
	}
	return false
}

func generateMutatePath(gvk schema.GroupVersionKind) string {
	return "/mutate-" + strings.ReplaceAll(gvk.Group, ".", "-") + "-" +
		gvk.Version + "-" + strings.ToLower(gvk.Kind)
}

func generateValidatePath(gvk schema.GroupVersionKind) string {
	return "/validate-" + strings.ReplaceAll(gvk.Group, ".", "-") + "-" +
		gvk.Version + "-" + strings.ToLower(gvk.Kind)
}
