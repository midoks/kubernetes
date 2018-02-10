/*
Copyright 2017 The Kubernetes Authors.

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

package vsphere

import (
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	. "github.com/onsi/gomega"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	vsphere "k8s.io/kubernetes/pkg/cloudprovider/providers/vsphere"
	"k8s.io/kubernetes/pkg/cloudprovider/providers/vsphere/vclib"
	"k8s.io/kubernetes/pkg/volume/util/volumehelper"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/storage/utils"

	"context"

	"github.com/vmware/govmomi/find"
	vimtypes "github.com/vmware/govmomi/vim25/types"
)

const (
	volumesPerNode = 55
	storageclass1  = "sc-default"
	storageclass2  = "sc-vsan"
	storageclass3  = "sc-spbm"
	storageclass4  = "sc-user-specified-ds"
)

// volumeState represents the state of a volume.
type volumeState int32

const (
	volumeStateDetached volumeState = 1
	volumeStateAttached volumeState = 2
)

// Sanity check for vSphere testing.  Verify the persistent disk attached to the node.
func verifyVSphereDiskAttached(c clientset.Interface, vsp *vsphere.VSphere, volumePath string, nodeName types.NodeName) (bool, error) {
	var (
		isAttached bool
		err        error
	)
	if vsp == nil {
		vsp, err = getVSphere(c)
		Expect(err).NotTo(HaveOccurred())
	}
	isAttached, err = vsp.DiskIsAttached(volumePath, nodeName)
	Expect(err).NotTo(HaveOccurred())
	return isAttached, err
}

// Wait until vsphere volumes are detached from the list of nodes or time out after 5 minutes
func waitForVSphereDisksToDetach(c clientset.Interface, vsp *vsphere.VSphere, nodeVolumes map[types.NodeName][]string) error {
	var (
		err            error
		disksAttached  = true
		detachTimeout  = 5 * time.Minute
		detachPollTime = 10 * time.Second
	)
	if vsp == nil {
		vsp, err = getVSphere(c)
		if err != nil {
			return err
		}
	}
	err = wait.Poll(detachPollTime, detachTimeout, func() (bool, error) {
		attachedResult, err := vsp.DisksAreAttached(nodeVolumes)
		if err != nil {
			return false, err
		}
		for nodeName, nodeVolumes := range attachedResult {
			for volumePath, attached := range nodeVolumes {
				if attached {
					framework.Logf("Waiting for volumes %q to detach from %q.", volumePath, string(nodeName))
					return false, nil
				}
			}
		}
		disksAttached = false
		framework.Logf("Volume are successfully detached from all the nodes: %+v", nodeVolumes)
		return true, nil
	})
	if err != nil {
		return err
	}
	if disksAttached {
		return fmt.Errorf("Gave up waiting for volumes to detach after %v", detachTimeout)
	}
	return nil
}

// Wait until vsphere vmdk moves to expected state on the given node, or time out after 6 minutes
func waitForVSphereDiskStatus(c clientset.Interface, vsp *vsphere.VSphere, volumePath string, nodeName types.NodeName, expectedState volumeState) error {
	var (
		err          error
		diskAttached bool
		currentState volumeState
		timeout      = 6 * time.Minute
		pollTime     = 10 * time.Second
	)

	var attachedState = map[bool]volumeState{
		true:  volumeStateAttached,
		false: volumeStateDetached,
	}

	var attachedStateMsg = map[volumeState]string{
		volumeStateAttached: "attached to",
		volumeStateDetached: "detached from",
	}

	err = wait.Poll(pollTime, timeout, func() (bool, error) {
		diskAttached, err = verifyVSphereDiskAttached(c, vsp, volumePath, nodeName)
		if err != nil {
			return true, err
		}

		currentState = attachedState[diskAttached]
		if currentState == expectedState {
			framework.Logf("Volume %q has successfully %s %q", volumePath, attachedStateMsg[currentState], nodeName)
			return true, nil
		}
		framework.Logf("Waiting for Volume %q to be %s %q.", volumePath, attachedStateMsg[expectedState], nodeName)
		return false, nil
	})
	if err != nil {
		return err
	}

	if currentState != expectedState {
		err = fmt.Errorf("Gave up waiting for Volume %q to be %s %q after %v", volumePath, attachedStateMsg[expectedState], nodeName, timeout)
	}
	return err
}

// Wait until vsphere vmdk is attached from the given node or time out after 6 minutes
func waitForVSphereDiskToAttach(c clientset.Interface, vsp *vsphere.VSphere, volumePath string, nodeName types.NodeName) error {
	return waitForVSphereDiskStatus(c, vsp, volumePath, nodeName, volumeStateAttached)
}

// Wait until vsphere vmdk is detached from the given node or time out after 6 minutes
func waitForVSphereDiskToDetach(c clientset.Interface, vsp *vsphere.VSphere, volumePath string, nodeName types.NodeName) error {
	return waitForVSphereDiskStatus(c, vsp, volumePath, nodeName, volumeStateDetached)
}

// function to create vsphere volume spec with given VMDK volume path, Reclaim Policy and labels
func getVSpherePersistentVolumeSpec(volumePath string, persistentVolumeReclaimPolicy v1.PersistentVolumeReclaimPolicy, labels map[string]string) *v1.PersistentVolume {
	var (
		pvConfig framework.PersistentVolumeConfig
		pv       *v1.PersistentVolume
		claimRef *v1.ObjectReference
	)
	pvConfig = framework.PersistentVolumeConfig{
		NamePrefix: "vspherepv-",
		PVSource: v1.PersistentVolumeSource{
			VsphereVolume: &v1.VsphereVirtualDiskVolumeSource{
				VolumePath: volumePath,
				FSType:     "ext4",
			},
		},
		Prebind: nil,
	}

	pv = &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: pvConfig.NamePrefix,
			Annotations: map[string]string{
				volumehelper.VolumeGidAnnotationKey: "777",
			},
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: persistentVolumeReclaimPolicy,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): resource.MustParse("2Gi"),
			},
			PersistentVolumeSource: pvConfig.PVSource,
			AccessModes: []v1.PersistentVolumeAccessMode{
				v1.ReadWriteOnce,
			},
			ClaimRef: claimRef,
		},
	}
	if labels != nil {
		pv.Labels = labels
	}
	return pv
}

// function to get vsphere persistent volume spec with given selector labels.
func getVSpherePersistentVolumeClaimSpec(namespace string, labels map[string]string) *v1.PersistentVolumeClaim {
	var (
		pvc *v1.PersistentVolumeClaim
	)
	pvc = &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pvc-",
			Namespace:    namespace,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{
				v1.ReadWriteOnce,
			},
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceName(v1.ResourceStorage): resource.MustParse("2Gi"),
				},
			},
		},
	}
	if labels != nil {
		pvc.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
	}

	return pvc
}

// function to create vmdk volume
func createVSphereVolume(vsp *vsphere.VSphere, volumeOptions *vclib.VolumeOptions) (string, error) {
	var (
		volumePath string
		err        error
	)
	if volumeOptions == nil {
		volumeOptions = new(vclib.VolumeOptions)
		volumeOptions.CapacityKB = 2097152
		volumeOptions.Name = "e2e-vmdk-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	volumePath, err = vsp.CreateVolume(volumeOptions)
	Expect(err).NotTo(HaveOccurred())
	return volumePath, nil
}

// CreateVSphereVolume creates a vmdk volume
func CreateVSphereVolume(vsp *vsphere.VSphere, volumeOptions *vclib.VolumeOptions) (string, error) {
	return createVSphereVolume(vsp, volumeOptions)
}

// function to write content to the volume backed by given PVC
func writeContentToVSpherePV(client clientset.Interface, pvc *v1.PersistentVolumeClaim, expectedContent string) {
	utils.RunInPodWithVolume(client, pvc.Namespace, pvc.Name, "echo "+expectedContent+" > /mnt/test/data")
	framework.Logf("Done with writing content to volume")
}

// function to verify content is matching on the volume backed for given PVC
func verifyContentOfVSpherePV(client clientset.Interface, pvc *v1.PersistentVolumeClaim, expectedContent string) {
	utils.RunInPodWithVolume(client, pvc.Namespace, pvc.Name, "grep '"+expectedContent+"' /mnt/test/data")
	framework.Logf("Successfully verified content of the volume")
}

func getVSphereStorageClassSpec(name string, scParameters map[string]string) *storage.StorageClass {
	var sc *storage.StorageClass

	sc = &storage.StorageClass{
		TypeMeta: metav1.TypeMeta{
			Kind: "StorageClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Provisioner: "kubernetes.io/vsphere-volume",
	}
	if scParameters != nil {
		sc.Parameters = scParameters
	}
	return sc
}

func getVSphereClaimSpecWithStorageClassAnnotation(ns string, diskSize string, storageclass *storage.StorageClass) *v1.PersistentVolumeClaim {
	scAnnotation := make(map[string]string)
	scAnnotation[v1.BetaStorageClassAnnotation] = storageclass.Name

	claim := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pvc-",
			Namespace:    ns,
			Annotations:  scAnnotation,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{
				v1.ReadWriteOnce,
			},
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceName(v1.ResourceStorage): resource.MustParse(diskSize),
				},
			},
		},
	}
	return claim
}

// func to get pod spec with given volume claim, node selector labels and command
func getVSpherePodSpecWithClaim(claimName string, nodeSelectorKV map[string]string, command string) *v1.Pod {
	pod := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pod-pvc-",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    "volume-tester",
					Image:   "busybox",
					Command: []string{"/bin/sh"},
					Args:    []string{"-c", command},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "my-volume",
							MountPath: "/mnt/test",
						},
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
			Volumes: []v1.Volume{
				{
					Name: "my-volume",
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							ClaimName: claimName,
							ReadOnly:  false,
						},
					},
				},
			},
		},
	}
	if nodeSelectorKV != nil {
		pod.Spec.NodeSelector = nodeSelectorKV
	}
	return pod
}

// func to get pod spec with given volume paths, node selector lables and container commands
func getVSpherePodSpecWithVolumePaths(volumePaths []string, keyValuelabel map[string]string, commands []string) *v1.Pod {
	var volumeMounts []v1.VolumeMount
	var volumes []v1.Volume

	for index, volumePath := range volumePaths {
		name := fmt.Sprintf("volume%v", index+1)
		volumeMounts = append(volumeMounts, v1.VolumeMount{Name: name, MountPath: "/mnt/" + name})
		vsphereVolume := new(v1.VsphereVirtualDiskVolumeSource)
		vsphereVolume.VolumePath = volumePath
		vsphereVolume.FSType = "ext4"
		volumes = append(volumes, v1.Volume{Name: name})
		volumes[index].VolumeSource.VsphereVolume = vsphereVolume
	}

	if commands == nil || len(commands) == 0 {
		commands = []string{
			"/bin/sh",
			"-c",
			"while true; do sleep 2; done",
		}
	}
	pod := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "vsphere-e2e-",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:         "vsphere-e2e-container-" + string(uuid.NewUUID()),
					Image:        "busybox",
					Command:      commands,
					VolumeMounts: volumeMounts,
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
			Volumes:       volumes,
		},
	}

	if keyValuelabel != nil {
		pod.Spec.NodeSelector = keyValuelabel
	}
	return pod
}

func verifyFilesExistOnVSphereVolume(namespace string, podName string, filePaths []string) {
	for _, filePath := range filePaths {
		_, err := framework.RunKubectl("exec", fmt.Sprintf("--namespace=%s", namespace), podName, "--", "/bin/ls", filePath)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to verify file: %q on the pod: %q", filePath, podName))
	}
}

func createEmptyFilesOnVSphereVolume(namespace string, podName string, filePaths []string) {
	for _, filePath := range filePaths {
		err := framework.CreateEmptyFileOnPod(namespace, podName, filePath)
		Expect(err).NotTo(HaveOccurred())
	}
}

// verify volumes are attached to the node and are accessible in pod
func verifyVSphereVolumesAccessible(c clientset.Interface, pod *v1.Pod, persistentvolumes []*v1.PersistentVolume, vsp *vsphere.VSphere) {
	nodeName := pod.Spec.NodeName
	namespace := pod.Namespace
	for index, pv := range persistentvolumes {
		// Verify disks are attached to the node
		isAttached, err := verifyVSphereDiskAttached(c, vsp, pv.Spec.VsphereVolume.VolumePath, types.NodeName(nodeName))
		Expect(err).NotTo(HaveOccurred())
		Expect(isAttached).To(BeTrue(), fmt.Sprintf("disk %v is not attached with the node", pv.Spec.VsphereVolume.VolumePath))
		// Verify Volumes are accessible
		filepath := filepath.Join("/mnt/", fmt.Sprintf("volume%v", index+1), "/emptyFile.txt")
		_, err = framework.LookForStringInPodExec(namespace, pod.Name, []string{"/bin/touch", filepath}, "", time.Minute)
		Expect(err).NotTo(HaveOccurred())
	}
}

// Get vSphere Volume Path from PVC
func getvSphereVolumePathFromClaim(client clientset.Interface, namespace string, claimName string) string {
	pvclaim, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(claimName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	pv, err := client.CoreV1().PersistentVolumes().Get(pvclaim.Spec.VolumeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	return pv.Spec.VsphereVolume.VolumePath
}

func addNodesToVCP(vsp *vsphere.VSphere, c clientset.Interface) error {
	nodes, err := c.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, node := range nodes.Items {
		vsp.NodeAdded(&node)
	}
	return nil
}

func getVSphere(c clientset.Interface) (*vsphere.VSphere, error) {
	vsp, err := vsphere.GetVSphere()
	if err != nil {
		return nil, err
	}
	addNodesToVCP(vsp, c)
	return vsp, nil
}

// GetVSphere returns vsphere cloud provider
func GetVSphere(c clientset.Interface) (*vsphere.VSphere, error) {
	return getVSphere(c)
}

// get .vmx file path for a virtual machine
func getVMXFilePath(vmObject *object.VirtualMachine) (vmxPath string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var nodeVM mo.VirtualMachine
	err := vmObject.Properties(ctx, vmObject.Reference(), []string{"config.files"}, &nodeVM)
	Expect(err).NotTo(HaveOccurred())
	Expect(nodeVM.Config).NotTo(BeNil())

	vmxPath = nodeVM.Config.Files.VmPathName
	framework.Logf("vmx file path is %s", vmxPath)
	return vmxPath
}

// verify ready node count. Try upto 3 minutes. Return true if count is expected count
func verifyReadyNodeCount(client clientset.Interface, expectedNodes int) bool {
	numNodes := 0
	for i := 0; i < 36; i++ {
		nodeList := framework.GetReadySchedulableNodesOrDie(client)
		Expect(nodeList.Items).NotTo(BeEmpty(), "Unable to find ready and schedulable Node")

		numNodes = len(nodeList.Items)
		if numNodes == expectedNodes {
			break
		}
		time.Sleep(5 * time.Second)
	}
	return (numNodes == expectedNodes)
}

// poweroff nodeVM and confirm the poweroff state
func poweroffNodeVM(nodeName string, vm *object.VirtualMachine) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	framework.Logf("Powering off node VM %s", nodeName)

	_, err := vm.PowerOff(ctx)
	Expect(err).NotTo(HaveOccurred())
	err = vm.WaitForPowerState(ctx, vimtypes.VirtualMachinePowerStatePoweredOff)
	Expect(err).NotTo(HaveOccurred(), "Unable to power off the node")
}

// poweron nodeVM and confirm the poweron state
func poweronNodeVM(nodeName string, vm *object.VirtualMachine) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	framework.Logf("Powering on node VM %s", nodeName)

	vm.PowerOn(ctx)
	err := vm.WaitForPowerState(ctx, vimtypes.VirtualMachinePowerStatePoweredOn)
	Expect(err).NotTo(HaveOccurred(), "Unable to power on the node")
}

// unregister a nodeVM from VC
func unregisterNodeVM(nodeName string, vm *object.VirtualMachine) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poweroffNodeVM(nodeName, vm)

	framework.Logf("Unregistering node VM %s", nodeName)
	err := vm.Unregister(ctx)
	Expect(err).NotTo(HaveOccurred(), "Unable to unregister the node")
}

// register a nodeVM into a VC
func registerNodeVM(nodeName, workingDir, vmxFilePath string, rpool *object.ResourcePool, host *object.HostSystem) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	framework.Logf("Registering node VM %s with vmx file path %s", nodeName, vmxFilePath)

	nodeInfo := TestContext.NodeMapper.GetNodeInfo(nodeName)
	finder := find.NewFinder(nodeInfo.VSphere.Client.Client, true)

	vmFolder, err := finder.FolderOrDefault(ctx, workingDir)
	Expect(err).NotTo(HaveOccurred())

	registerTask, err := vmFolder.RegisterVM(ctx, vmxFilePath, nodeName, false, rpool, host)
	Expect(err).NotTo(HaveOccurred())
	err = registerTask.Wait(ctx)
	Expect(err).NotTo(HaveOccurred())

	vmPath := filepath.Join(workingDir, nodeName)
	vm, err := finder.VirtualMachine(ctx, vmPath)
	Expect(err).NotTo(HaveOccurred())

	poweronNodeVM(nodeName, vm)
}
