/*
   MIT License

   Copyright (c) Microsoft Corporation.

   Permission is hereby granted, free of charge, to any person obtaining a copy
   of this software and associated documentation files (the "Software"), to deal
   in the Software without restriction, including without limitation the rights
   to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
   copies of the Software, and to permit persons to whom the Software is
   furnished to do so, subject to the following conditions:

   The above copyright notice and this permission notice shall be included in all
   copies or substantial portions of the Software.

   THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
   IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
   FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
   AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
   LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
   OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
   SOFTWARE
*/

package e2e

import (
	"context"
	"net"
	"regexp"

	network "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientset "k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/kube-egress-gateway/api/v1alpha1"
	"github.com/Azure/kube-egress-gateway/e2e/utils"
	"github.com/Azure/kube-egress-gateway/pkg/azureclients"
	"github.com/Azure/kube-egress-gateway/pkg/utils/to"
)

var (
	podIPRE     = regexp.MustCompile(`((25[0-5]|(2[0-4]|1\d|[1-9]|)\d)\.?\b){4}`)
	nginxRespRE = regexp.MustCompile(`Welcome to nginx!`)
)

var _ = Describe("Test staticGatewayConfiguration deployment", func() {
	// use controller-runtime client to manage cr
	var k8sClient client.Client
	// there is no easy way to get pod log with controller-runtime client, use client-go client instead
	var podLogClient clientset.Interface
	var azureClientFactory azureclients.AzureClientsFactory
	var testns string

	BeforeEach(func() {
		var err error
		k8sClient, podLogClient, err = utils.CreateK8sClient()
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient).NotTo(BeNil())
		Expect(podLogClient).NotTo(BeNil())
		azureClientFactory, err = utils.CreateAzureClients()
		Expect(err).NotTo(HaveOccurred())
		testns = genTestNamespace()
		err = utils.CreateNamespace(testns, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Created ns: %s", testns)
	})

	AfterEach(func() {
		err := utils.DeleteNamespace(testns, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Deleted ns: %s", testns)
		k8sClient = nil
		podLogClient = nil
		testns = ""
	})

	It("should let pod egress from the egress gateway", func() {
		rg, vmss, _, prefixLen, err := utils.GetGatewayVmssProfile(k8sClient)
		Expect(err).NotTo(HaveOccurred())

		By("Creating a StaticGatewayConfiguration")
		sgw := &v1alpha1.StaticGatewayConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sgw1",
				Namespace: testns,
			},
			Spec: v1alpha1.StaticGatewayConfigurationSpec{
				GatewayVMSSProfile: v1alpha1.GatewayVMSSProfile{
					VMSSResourceGroup:  rg,
					VMSSName:           vmss,
					PublicIpPrefixSize: prefixLen,
				},
			},
		}
		err = utils.CreateK8sObject(sgw, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		pipPrefix, err := utils.WaitStaticGatewayProvision(sgw, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Got egress gateway prefix: %s", pipPrefix)

		By("Creating a test pod")
		pod := utils.CreateCurlPodManifest(testns, "sgw1", "ifconfig.me")
		err = utils.CreateK8sObject(pod, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		podEgressIP, err := utils.GetExpectedPodLog(pod, podLogClient, podIPRE)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Get pod egress IP: %s", podEgressIP)

		By("Checking pod egress IP belongs to egress gateway outbound IP range")
		_, ipNet, _ := net.ParseCIDR(pipPrefix)
		Expect(ipNet.Contains(net.ParseIP(podEgressIP))).To(BeTrue())
	})

	It("should not affect pod not using egress gateway", func() {
		rg, vmss, _, prefixLen, err := utils.GetGatewayVmssProfile(k8sClient)
		Expect(err).NotTo(HaveOccurred())

		By("Creating a StaticGatewayConfiguration")
		sgw := &v1alpha1.StaticGatewayConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sgw1",
				Namespace: testns,
			},
			Spec: v1alpha1.StaticGatewayConfigurationSpec{
				GatewayVMSSProfile: v1alpha1.GatewayVMSSProfile{
					VMSSResourceGroup:  rg,
					VMSSName:           vmss,
					PublicIpPrefixSize: prefixLen,
				},
			},
		}
		err = utils.CreateK8sObject(sgw, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		pipPrefix, err := utils.WaitStaticGatewayProvision(sgw, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Got egress gateway prefix: %s", pipPrefix)

		By("Creating a test pod NOT using egress gateway")
		pod := utils.CreateCurlPodManifest(testns, "", "ifconfig.me")
		err = utils.CreateK8sObject(pod, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		podEgressIP, err := utils.GetExpectedPodLog(pod, podLogClient, podIPRE)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Get pod egress IP: %s", podEgressIP)

		By("Checking pod egress IP DOES NOT belong to egress gateway outbound IP range")
		_, ipNet, _ := net.ParseCIDR(pipPrefix)
		Expect(ipNet.Contains(net.ParseIP(podEgressIP))).To(BeFalse())
	})

	It("should support BYO public ip prefix as gateway configuration", func() {
		rg, vmss, loc, prefixLen, err := utils.GetGatewayVmssProfile(k8sClient)
		Expect(err).NotTo(HaveOccurred())
		By("Creating a pip prefix")
		pipPrefixClient, err := azureClientFactory.GetPublicIPPrefixesClient()
		Expect(err).NotTo(HaveOccurred())
		prefixName := "test-prefix-" + string(uuid.NewUUID())[0:4]
		testPrefix := network.PublicIPPrefix{
			Name:     to.Ptr(prefixName),
			Location: to.Ptr(loc),
			Properties: &network.PublicIPPrefixPropertiesFormat{
				PrefixLength:           to.Ptr(prefixLen),
				PublicIPAddressVersion: to.Ptr(network.IPVersionIPv4),
			},
			SKU: &network.PublicIPPrefixSKU{
				Name: to.Ptr(network.PublicIPPrefixSKUNameStandard),
				Tier: to.Ptr(network.PublicIPPrefixSKUTierRegional),
			},
		}
		prefix, err := pipPrefixClient.CreateOrUpdate(context.Background(), rg, prefixName, testPrefix)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			err := utils.WaitPipPrefixDeletion(rg, prefixName, pipPrefixClient)
			Expect(err).NotTo(HaveOccurred())
		}()
		utils.Logf("Got BYO pip prefix: %s", to.Val(prefix.Properties.IPPrefix))

		By("Creating a StaticGatewayConfiguration using BYO pip prefix")
		sgw := &v1alpha1.StaticGatewayConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sgw1",
				Namespace: testns,
			},
			Spec: v1alpha1.StaticGatewayConfigurationSpec{
				GatewayVMSSProfile: v1alpha1.GatewayVMSSProfile{
					VMSSResourceGroup:  rg,
					VMSSName:           vmss,
					PublicIpPrefixSize: prefixLen,
				},
				PublicIpPrefixId: to.Val(prefix.ID),
			},
		}
		err = utils.CreateK8sObject(sgw, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		pipPrefix, err := utils.WaitStaticGatewayProvision(sgw, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Got egress gateway prefix: %s", pipPrefix)
		Expect(pipPrefix).To(Equal(to.Val(prefix.Properties.IPPrefix)))
		utils.Logf("Deleting StaticGatewayConfiguration")
		err = utils.WaitStaticGatewayDeletion(sgw, k8sClient)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should not affect pod ingress when gateway is in use", func() {
		rg, vmss, _, prefixLen, err := utils.GetGatewayVmssProfile(k8sClient)
		Expect(err).NotTo(HaveOccurred())

		By("Creating a StaticGatewayConfiguration")
		sgw := &v1alpha1.StaticGatewayConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sgw1",
				Namespace: testns,
			},
			Spec: v1alpha1.StaticGatewayConfigurationSpec{
				GatewayVMSSProfile: v1alpha1.GatewayVMSSProfile{
					VMSSResourceGroup:  rg,
					VMSSName:           vmss,
					PublicIpPrefixSize: prefixLen,
				},
			},
		}
		err = utils.CreateK8sObject(sgw, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.WaitStaticGatewayProvision(sgw, k8sClient)
		Expect(err).NotTo(HaveOccurred())

		By("Creating an Nginx pod with gateway in use")
		pod := utils.CreateNginxPodManifest(testns, "sgw1")
		err = utils.CreateK8sObject(pod, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		podIP, err := utils.WaitGetPodIP(pod, podLogClient)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("nginx pod IP: %s", podIP)

		By("Creating a test pod to curl nginx's ip")
		pod2 := utils.CreateCurlPodManifest(testns, "", podIP)
		err = utils.CreateK8sObject(pod2, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.GetExpectedPodLog(pod2, podLogClient, nginxRespRE)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should exclude specified CIDRs from gateway", func() {
		By("Getting ifconfig.me ip address")
		pips, err := net.LookupIP("ifconfig.me")
		Expect(err).NotTo(HaveOccurred())
		Expect(pips).NotTo(BeEmpty())
		utils.Logf("ifconfig.me ips: %v", pips)
		var cidrs []string
		for _, pip := range pips {
			if pip.To4() != nil {
				cidrs = append(cidrs, pip.String()+"/32")
			}
		}

		By("Creating a StaticGatewayConfiguration excluding ifconfig.me's ip")
		rg, vmss, _, prefixLen, err := utils.GetGatewayVmssProfile(k8sClient)
		Expect(err).NotTo(HaveOccurred())
		sgw := &v1alpha1.StaticGatewayConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sgw1",
				Namespace: testns,
			},
			Spec: v1alpha1.StaticGatewayConfigurationSpec{
				GatewayVMSSProfile: v1alpha1.GatewayVMSSProfile{
					VMSSResourceGroup:  rg,
					VMSSName:           vmss,
					PublicIpPrefixSize: prefixLen,
				},
				ExcludeCIDRs: cidrs,
			},
		}
		err = utils.CreateK8sObject(sgw, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		pipPrefix, err := utils.WaitStaticGatewayProvision(sgw, k8sClient)
		Expect(err).NotTo(HaveOccurred())

		By("Creating a test pod using egress gateway")
		pod := utils.CreateCurlPodManifest(testns, "sgw1", "ifconfig.me")
		err = utils.CreateK8sObject(pod, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		podEgressIP, err := utils.GetExpectedPodLog(pod, podLogClient, podIPRE)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Get pod egress IP: %s", podEgressIP)

		By("Checking pod egress IP DOES NOT belong to egress gateway outbound IP range")
		_, ipNet, _ := net.ParseCIDR(pipPrefix)
		Expect(ipNet.Contains(net.ParseIP(podEgressIP))).To(BeFalse())
	})

	It("should support multiple gateways and pods", func() {
		By("Creating two StaticGatewayConfigurations")
		rg, vmss, _, prefixLen, err := utils.GetGatewayVmssProfile(k8sClient)
		Expect(err).NotTo(HaveOccurred())
		prefixes := make(map[string]string)
		names := []string{"sgw1", "sgw2"}
		for _, name := range names {
			sgw := &v1alpha1.StaticGatewayConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: testns,
				},
				Spec: v1alpha1.StaticGatewayConfigurationSpec{
					GatewayVMSSProfile: v1alpha1.GatewayVMSSProfile{
						VMSSResourceGroup:  rg,
						VMSSName:           vmss,
						PublicIpPrefixSize: prefixLen,
					},
				},
			}
			utils.Logf("Creating gateway %s", name)
			err = utils.CreateK8sObject(sgw, k8sClient)
			Expect(err).NotTo(HaveOccurred())
			pipPrefix, err := utils.WaitStaticGatewayProvision(sgw, k8sClient)
			Expect(err).NotTo(HaveOccurred())
			prefixes[name] = pipPrefix
			utils.Logf("Gateway %s has egress cidr %s", name, pipPrefix)
		}

		By("Creating test pod using the gateways")
		for _, name := range names {
			pod := utils.CreateCurlPodManifest(testns, name, "ifconfig.me")
			err = utils.CreateK8sObject(pod, k8sClient)
			Expect(err).NotTo(HaveOccurred())
			podEgressIP, err := utils.GetExpectedPodLog(pod, podLogClient, podIPRE)
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("Get pod egress IP: %s", podEgressIP)

			By("Checking pod egress IP belongs to egress gateway outbound IP range")
			_, ipNet, _ := net.ParseCIDR(prefixes[name])
			Expect(ipNet.Contains(net.ParseIP(podEgressIP))).To(BeTrue())
		}
	})
})

func genTestNamespace() string {
	return "e2e-test-" + string(uuid.NewUUID())[0:4]
}
