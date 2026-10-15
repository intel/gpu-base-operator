/*
Copyright 2026 Intel Corporation. All Rights Reserved.

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

package common

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	SCCAPIVersion = "security.openshift.io/v1"
	SCCKind       = "SecurityContextConstraints"
)

// BuildSCC constructs an OpenShift SecurityContextConstraints unstructured object.
func BuildSCC(name string, spec map[string]interface{}) *unstructured.Unstructured {
	scc := &unstructured.Unstructured{}
	scc.SetAPIVersion(SCCAPIVersion)
	scc.SetKind(SCCKind)
	scc.SetName(name)

	for k, v := range spec {
		scc.Object[k] = v
	}

	return scc
}

func BuildOpenShiftNames(crName, componentName string) (sccName string, roleName string, bindingName string, saName string) {
	sccName = fmt.Sprintf("%s-%s-scc", crName, componentName)
	roleName = fmt.Sprintf("%s-%s-scc-role", crName, componentName)
	bindingName = fmt.Sprintf("%s-%s-scc-binding", crName, componentName)
	saName = fmt.Sprintf("%s-%s-sa", crName, componentName)

	return sccName, roleName, bindingName, saName
}

func EnsureSCC(ctx context.Context, c client.Client, scc *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetAPIVersion(SCCAPIVersion)
	existing.SetKind(SCCKind)

	err := c.Get(ctx, client.ObjectKey{Name: scc.GetName()}, existing)
	if errors.IsNotFound(err) {
		if createErr := c.Create(ctx, scc); createErr != nil && !errors.IsAlreadyExists(createErr) {
			return createErr
		}

		return nil
	}

	if err != nil {
		return err
	}

	scc.SetResourceVersion(existing.GetResourceVersion())

	return c.Update(ctx, scc)
}

// CreateSCCRole creates a ClusterRole that allows using the named SCC if it does not already exist.
func CreateSCCRole(ctx context.Context, c client.Client, roleName, sccName string) error {
	cr := &rbac.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: roleName},
		Rules: []rbac.PolicyRule{{
			APIGroups:     []string{"security.openshift.io"},
			Resources:     []string{"securitycontextconstraints"},
			ResourceNames: []string{sccName},
			Verbs:         []string{"use"},
		}},
	}

	if _, err := controllerutil.CreateOrPatch(ctx, c, cr, func() error {
		cr.Rules = []rbac.PolicyRule{{
			APIGroups:     []string{"security.openshift.io"},
			Resources:     []string{"securitycontextconstraints"},
			ResourceNames: []string{sccName},
			Verbs:         []string{"use"},
		}}

		return nil
	}); err != nil {
		return fmt.Errorf("failed to create or patch ClusterRole %s: %v", roleName, err)
	}

	return nil
}

// CreateSCCRoleBinding creates a ClusterRoleBinding that binds the ServiceAccount to the SCC ClusterRole
// if the binding does not already exist.
func CreateSCCRoleBinding(ctx context.Context, c client.Client, bindingName, roleName, saName, namespace string) error {
	crb := &rbac.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingName},
		RoleRef: rbac.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     roleName,
		},
		Subjects: []rbac.Subject{{
			Kind:      "ServiceAccount",
			Name:      saName,
			Namespace: namespace,
		}},
	}

	if _, err := controllerutil.CreateOrPatch(ctx, c, crb, func() error {
		crb.RoleRef = rbac.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     roleName,
		}
		crb.Subjects = []rbac.Subject{{
			Kind:      "ServiceAccount",
			Name:      saName,
			Namespace: namespace,
		}}

		return nil
	}); err != nil {
		return fmt.Errorf("failed to create or patch ClusterRoleBinding %s: %v", bindingName, err)
	}

	return nil
}

// CreateServiceAccount creates a namespace-scoped ServiceAccount if it does not already exist.
func CreateServiceAccount(ctx context.Context, c client.Client, name, namespace string) error {
	sa := &core.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}

	existing := &core.ServiceAccount{}

	err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, existing)
	if errors.IsNotFound(err) {
		if createErr := c.Create(ctx, sa); createErr != nil && !errors.IsAlreadyExists(createErr) {
			return createErr
		}

		return nil
	}

	return err
}

// DeleteOpenShiftSCCResources deletes all OpenShift SCC-related resources for a component.
// Each resource is deleted independently; NotFound errors are ignored.
// Pass an empty saName or namespace to skip ServiceAccount deletion.
func DeleteOpenShiftSCCResources(ctx context.Context, c client.Client, sccName, roleName, bindingName, saName, namespace string) {
	scc := &unstructured.Unstructured{}
	scc.SetAPIVersion(SCCAPIVersion)
	scc.SetKind(SCCKind)
	scc.SetName(sccName)

	if err := c.Delete(ctx, scc); err != nil && !errors.IsNotFound(err) {
		klog.Errorf("Failed to delete SCC %s: %v", sccName, err)
	}

	cr := &rbac.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: roleName}}
	if err := c.Delete(ctx, cr); err != nil && !errors.IsNotFound(err) {
		klog.Errorf("Failed to delete ClusterRole %s: %v", roleName, err)
	}

	crb := &rbac.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: bindingName}}
	if err := c.Delete(ctx, crb); err != nil && !errors.IsNotFound(err) {
		klog.Errorf("Failed to delete ClusterRoleBinding %s: %v", bindingName, err)
	}

	if saName != "" && namespace != "" {
		sa := &core.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: namespace}}
		if err := c.Delete(ctx, sa); err != nil && !errors.IsNotFound(err) {
			klog.Errorf("Failed to delete ServiceAccount %s: %v", saName, err)
		}
	}
}
