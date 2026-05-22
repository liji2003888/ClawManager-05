package k8s

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// hostPathPVCFallbackEnabled reports whether the manual node-local hostPath PV
// fallback is allowed. It is disabled by default because PVCs are provisioned by
// an external StorageClass; opt in with K8S_PVC_HOSTPATH_FALLBACK=true only for
// clusters without a working dynamic provisioner (csotai: c93d67c).
func hostPathPVCFallbackEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("K8S_PVC_HOSTPATH_FALLBACK"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// PVCService handles PersistentVolumeClaim operations
type PVCService struct {
	client           *Client
	namespaceService *NamespaceService
}

// NewPVCService creates a new PVC service
func NewPVCService() *PVCService {
	return &PVCService{
		client:           globalClient,
		namespaceService: NewNamespaceService(),
	}
}

// GetClient returns the k8s client
func (s *PVCService) GetClient() *Client {
	return s.client
}

// CreatePVC creates a new PVC for an instance
func (s *PVCService) CreatePVC(ctx context.Context, userID, instanceID int, storageSizeGB int, storageClass string) (*corev1.PersistentVolumeClaim, error) {
	if s.client == nil {
		return nil, fmt.Errorf("k8s client not initialized")
	}

	// Ensure namespace exists
	if _, err := s.namespaceService.EnsureNamespace(ctx, userID); err != nil {
		return nil, fmt.Errorf("failed to ensure namespace: %w", err)
	}

	pvcName := s.client.GetPVCName(instanceID)
	namespace := s.client.GetNamespace(userID)

	// Use default storage class if not specified
	if storageClass == "" {
		storageClass = s.client.StorageClass
	}

	fmt.Printf("Creating PVC %s in namespace %s with storageClass: %s\n", pvcName, namespace, storageClass)

	storageSize := resource.MustParse(fmt.Sprintf("%dGi", storageSizeGB))

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: namespace,
			Labels: map[string]string{
				"app":         "clawreef",
				"instance-id": fmt.Sprintf("%d", instanceID),
				"managed-by":  "clawreef",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storageSize,
				},
			},
		},
	}

	// A nil StorageClassName makes Kubernetes use the cluster default class; a
	// pointer to "" instead *disables* the default class, leaving the PVC
	// permanently Pending (now that the hostPath fallback is off by default).
	// (csotai customization: c93d67c)
	if storageClass != "" {
		pvc.Spec.StorageClassName = &storageClass
	}

	createdPVC, err := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		// Check if PVC already exists
		if errors.IsAlreadyExists(err) {
			// Try to get the existing PVC
			existingPVC, getErr := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
			if getErr == nil && existingPVC != nil {
				// Check if PVC belongs to the same instance
				if existingPVC.Labels["instance-id"] == fmt.Sprintf("%d", instanceID) {
					// PVC exists and belongs to this instance, return it
					return existingPVC, nil
				}
				// PVC exists but belongs to a different instance, delete it and recreate
				deleteErr := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, pvcName, metav1.DeleteOptions{})
				if deleteErr != nil && !errors.IsNotFound(deleteErr) {
					return nil, fmt.Errorf("failed to delete existing PVC %s: %w", pvcName, deleteErr)
				}
				// Wait a moment for deletion to complete
				select {
				case <-time.After(2 * time.Second):
				}
				// Retry creation
				createdPVC, err = s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
				if err != nil {
					return nil, fmt.Errorf("failed to create PVC after deletion %s: %w", pvcName, err)
				}
				return createdPVC, nil
			}
		}
		return nil, fmt.Errorf("failed to create PVC %s: %w", pvcName, err)
	}

	// Wait for PVC to be bound, if not bound within timeout, create PV manually
	fmt.Printf("PVC %s created, scheduling async binding monitor...\n", pvcName)
	go s.monitorPVCBinding(context.Background(), namespace, pvcName, userID, instanceID, storageSizeGB, storageClass, 15*time.Second)

	return createdPVC, nil
}

// CreateTeamSharedPVC creates the RWX PVC mounted by every member of a Team.
func (s *PVCService) CreateTeamSharedPVC(ctx context.Context, userID, teamID, storageSizeGB int, storageClass string) (*corev1.PersistentVolumeClaim, error) {
	if s.client == nil {
		return nil, fmt.Errorf("k8s client not initialized")
	}
	if storageSizeGB <= 0 {
		storageSizeGB = 10
	}

	if _, err := s.namespaceService.EnsureNamespace(ctx, userID); err != nil {
		return nil, fmt.Errorf("failed to ensure namespace: %w", err)
	}

	pvcName := s.client.GetTeamSharedPVCName(teamID)
	namespace := s.client.GetNamespace(userID)
	if storageClass == "" {
		storageClass = s.client.StorageClass
	}
	storageSize := resource.MustParse(fmt.Sprintf("%dGi", storageSizeGB))

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: namespace,
			Labels: map[string]string{
				"app":        "clawreef",
				"team-id":    fmt.Sprintf("%d", teamID),
				"managed-by": "clawreef",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storageSize,
				},
			},
			StorageClassName: &storageClass,
		},
	}

	createdPVC, err := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			existingPVC, getErr := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
			if getErr == nil && existingPVC != nil && existingPVC.Labels["team-id"] == fmt.Sprintf("%d", teamID) {
				if existingPVC.Status.Phase != corev1.ClaimBound {
					go s.monitorTeamSharedPVCBinding(context.Background(), namespace, pvcName, userID, teamID, storageSizeGB, storageClass, 15*time.Second)
				}
				return existingPVC, nil
			}
		}
		return nil, fmt.Errorf("failed to create Team shared PVC %s: %w", pvcName, err)
	}

	go s.monitorTeamSharedPVCBinding(context.Background(), namespace, pvcName, userID, teamID, storageSizeGB, storageClass, 15*time.Second)
	return createdPVC, nil
}

func (s *PVCService) monitorTeamSharedPVCBinding(ctx context.Context, namespace, pvcName string, userID, teamID, storageSizeGB int, storageClass string, timeout time.Duration) {
	if _, err := s.waitForTeamSharedPVCBinding(ctx, namespace, pvcName, userID, teamID, storageSizeGB, storageClass, timeout); err != nil {
		fmt.Printf("Async Team shared PVC binding monitor failed for %s: %v\n", pvcName, err)
	}
}

func (s *PVCService) waitForTeamSharedPVCBinding(ctx context.Context, namespace, pvcName string, userID, teamID, storageSizeGB int, storageClass string, timeout time.Duration) (*corev1.PersistentVolumeClaim, error) {
	pvc, err := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get Team shared PVC %s: %w", pvcName, err)
	}
	if pvc.Status.Phase == corev1.ClaimBound {
		return pvc, nil
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	timeoutChan := time.After(timeout)

	for {
		select {
		case <-timeoutChan:
			fmt.Printf("Team shared PVC %s binding timeout, creating hostPath RWX PV manually\n", pvcName)
			return s.createPVForTeamSharedPVC(ctx, namespace, pvcName, userID, teamID, storageSizeGB, storageClass)
		case <-ticker.C:
			pvc, err := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to get Team shared PVC %s during wait: %w", pvcName, err)
			}
			if pvc.Status.Phase == corev1.ClaimBound {
				fmt.Printf("Team shared PVC %s bound successfully to %s\n", pvcName, pvc.Spec.VolumeName)
				return pvc, nil
			}
			fmt.Printf("Waiting for Team shared PVC %s binding, current status: %s\n", pvcName, pvc.Status.Phase)
		}
	}
}

func (s *PVCService) createPVForTeamSharedPVC(ctx context.Context, namespace, pvcName string, userID, teamID, storageSizeGB int, storageClass string) (*corev1.PersistentVolumeClaim, error) {
	pvName := fmt.Sprintf("clawreef-pv-user-%d-team-%d-shared", userID, teamID)
	hostPathPrefix := "/data/clawreef"
	if s.client != nil && s.client.HostPathPrefix != "" {
		hostPathPrefix = s.client.HostPathPrefix
	}
	hostPath := fmt.Sprintf("%s/user-%d/team-%d-shared", hostPathPrefix, userID, teamID)

	existingPV, err := s.client.Clientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err == nil && existingPV != nil {
		if existingPV.Status.Phase == corev1.VolumeReleased {
			if deleteErr := s.client.Clientset.CoreV1().PersistentVolumes().Delete(ctx, pvName, metav1.DeleteOptions{}); deleteErr != nil && !errors.IsNotFound(deleteErr) {
				return nil, fmt.Errorf("failed to delete released Team shared PV %s: %w", pvName, deleteErr)
			}
			time.Sleep(3 * time.Second)
		} else if existingPV.Spec.ClaimRef != nil &&
			existingPV.Spec.ClaimRef.Namespace == namespace &&
			existingPV.Spec.ClaimRef.Name == pvcName {
			time.Sleep(2 * time.Second)
			return s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
		} else if existingPV.Spec.ClaimRef != nil {
			return nil, fmt.Errorf("Team shared PV %s already belongs to %s/%s", pvName, existingPV.Spec.ClaimRef.Namespace, existingPV.Spec.ClaimRef.Name)
		}
	}

	pvc, err := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get Team shared PVC %s for UID: %w", pvcName, err)
	}
	storageSize := resource.MustParse(fmt.Sprintf("%dGi", storageSizeGB))
	nodeAffinity, err := s.hostPathPVNodeAffinity(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to select node for Team shared hostPath PV %s: %w", pvName, err)
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			Labels: map[string]string{
				"app":        "clawreef",
				"user-id":    fmt.Sprintf("%d", userID),
				"team-id":    fmt.Sprintf("%d", teamID),
				"managed-by": "clawreef",
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: storageSize,
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany,
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              storageClass,
			NodeAffinity:                  nodeAffinity,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: hostPath,
					Type: func() *corev1.HostPathType {
						t := corev1.HostPathDirectoryOrCreate
						return &t
					}(),
				},
			},
			ClaimRef: &corev1.ObjectReference{
				Kind:            "PersistentVolumeClaim",
				APIVersion:      "v1",
				Namespace:       namespace,
				Name:            pvcName,
				UID:             pvc.UID,
				ResourceVersion: pvc.ResourceVersion,
			},
		},
	}
	if _, err := s.client.Clientset.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("failed to create Team shared PV %s: %w", pvName, err)
	}

	time.Sleep(3 * time.Second)
	pvc, err = s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get Team shared PVC after PV creation: %w", err)
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		return nil, fmt.Errorf("Team shared PVC %s is still not bound after PV creation, status: %s", pvcName, pvc.Status.Phase)
	}
	fmt.Printf("Team shared PVC %s successfully bound to PV %s\n", pvcName, pvName)
	return pvc, nil
}

func (s *PVCService) hostPathPVNodeAffinity(ctx context.Context) (*corev1.VolumeNodeAffinity, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("k8s client not initialized")
	}
	nodes, err := s.client.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	type candidate struct {
		name     string
		hostname string
	}
	candidates := []candidate{}
	for _, node := range nodes.Items {
		if node.Spec.Unschedulable || !isStorageNodeReady(node) {
			continue
		}
		hostname := strings.TrimSpace(node.Labels["kubernetes.io/hostname"])
		if hostname == "" {
			hostname = strings.TrimSpace(node.Name)
		}
		if hostname == "" {
			continue
		}
		candidates = append(candidates, candidate{name: node.Name, hostname: hostname})
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no ready node found for hostPath PV")
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].name < candidates[j].name
	})
	return hostPathPVNodeAffinityForHostname(candidates[0].hostname), nil
}

func hostPathPVNodeAffinityForHostname(hostname string) *corev1.VolumeNodeAffinity {
	return &corev1.VolumeNodeAffinity{
		Required: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{
							Key:      "kubernetes.io/hostname",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{hostname},
						},
					},
				},
			},
		},
	}
}

func isStorageNodeReady(node corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (s *PVCService) monitorPVCBinding(ctx context.Context, namespace, pvcName string, userID, instanceID, storageSizeGB int, storageClass string, timeout time.Duration) {
	if _, err := s.waitForPVCBinding(ctx, namespace, pvcName, userID, instanceID, storageSizeGB, storageClass, timeout); err != nil {
		fmt.Printf("Async PVC binding monitor failed for %s: %v\n", pvcName, err)
	}
}

// waitForPVCBinding waits for PVC to be bound, if timeout creates PV manually
func (s *PVCService) waitForPVCBinding(ctx context.Context, namespace, pvcName string, userID, instanceID, storageSizeGB int, storageClass string, timeout time.Duration) (*corev1.PersistentVolumeClaim, error) {
	// Check if PVC is already bound
	pvc, err := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get PVC %s: %w", pvcName, err)
	}

	if pvc.Status.Phase == corev1.ClaimBound {
		fmt.Printf("PVC %s is already bound to %s\n", pvcName, pvc.Spec.VolumeName)
		return pvc, nil
	}

	// Wait for binding with timeout
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	timeoutChan := time.After(timeout)

	for {
		select {
		case <-timeoutChan:
			// A StorageClass-backed PVC (external NFS) must be bound by its
			// provisioner; with WaitForFirstConsumer it stays Pending until the
			// pod is scheduled, so substituting a node-local hostPath PV here
			// would silently move "persistent" data onto node disk. Only fall
			// back to a manual hostPath PV when explicitly opted in.
			// (csotai customization: c93d67c)
			if !hostPathPVCFallbackEnabled() {
				fmt.Printf("PVC %s not bound within %s; leaving it to the provisioner (hostPath fallback disabled)\n", pvcName, timeout)
				return nil, fmt.Errorf("PVC %s not bound within %s and hostPath fallback is disabled (set K8S_PVC_HOSTPATH_FALLBACK=true to enable)", pvcName, timeout)
			}
			fmt.Printf("PVC %s binding timeout, creating PV manually\n", pvcName)
			return s.createPVForPVC(ctx, namespace, pvcName, userID, instanceID, storageSizeGB, storageClass)
		case <-ticker.C:
			pvc, err := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to get PVC %s during wait: %w", pvcName, err)
			}
			if pvc.Status.Phase == corev1.ClaimBound {
				fmt.Printf("PVC %s bound successfully to %s\n", pvcName, pvc.Spec.VolumeName)
				return pvc, nil
			}
			fmt.Printf("Waiting for PVC %s binding, current status: %s\n", pvcName, pvc.Status.Phase)
		}
	}
}

// createPVForPVC creates a PV manually to bind to the PVC
func (s *PVCService) createPVForPVC(ctx context.Context, namespace, pvcName string, userID, instanceID, storageSizeGB int, storageClass string) (*corev1.PersistentVolumeClaim, error) {
	pvName := fmt.Sprintf("clawreef-pv-user-%d-instance-%d", userID, instanceID)
	// Use configurable host path prefix for persistent storage
	hostPathPrefix := "/data/clawreef"
	if s.client != nil && s.client.HostPathPrefix != "" {
		hostPathPrefix = s.client.HostPathPrefix
	}
	hostPath := fmt.Sprintf("%s/user-%d/instance-%d", hostPathPrefix, userID, instanceID)

	fmt.Printf("Creating PV %s with hostPath %s for PVC %s\n", pvName, hostPath, pvcName)

	// Check if PV already exists
	existingPV, err := s.client.Clientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err == nil && existingPV != nil {
		fmt.Printf("PV %s already exists (status: %s), checking if it's bound to our PVC\n", pvName, existingPV.Status.Phase)

		// Check if PV is in Released state - if so, we need to delete and recreate it
		if existingPV.Status.Phase == corev1.VolumeReleased {
			fmt.Printf("PV %s is in Released state, deleting it to recreate\n", pvName)
			deleteErr := s.client.Clientset.CoreV1().PersistentVolumes().Delete(ctx, pvName, metav1.DeleteOptions{})
			if deleteErr != nil && !errors.IsNotFound(deleteErr) {
				return nil, fmt.Errorf("failed to delete released PV %s: %w", pvName, deleteErr)
			}
			// Wait for PV deletion
			time.Sleep(3 * time.Second)
		} else if existingPV.Spec.ClaimRef != nil &&
			existingPV.Spec.ClaimRef.Namespace == namespace &&
			existingPV.Spec.ClaimRef.Name == pvcName {
			// PV is already bound to our PVC, wait a bit for binding to complete
			time.Sleep(2 * time.Second)
			pvc, err := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to get PVC after existing PV check: %w", err)
			}
			return pvc, nil
		} else if existingPV.Spec.ClaimRef != nil {
			// PV exists but bound to different PVC, delete it
			fmt.Printf("PV %s exists but bound to different claim (%s/%s), deleting it\n",
				pvName, existingPV.Spec.ClaimRef.Namespace, existingPV.Spec.ClaimRef.Name)
			deleteErr := s.client.Clientset.CoreV1().PersistentVolumes().Delete(ctx, pvName, metav1.DeleteOptions{})
			if deleteErr != nil && !errors.IsNotFound(deleteErr) {
				return nil, fmt.Errorf("failed to delete existing PV %s: %w", pvName, deleteErr)
			}
			// Wait for PV deletion
			time.Sleep(3 * time.Second)
		}
	}

	// Get PVC to get its UID
	pvc, err := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get PVC %s for UID: %w", pvcName, err)
	}

	storageSize := resource.MustParse(fmt.Sprintf("%dGi", storageSizeGB))

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			Labels: map[string]string{
				"app":         "clawreef",
				"user-id":     fmt.Sprintf("%d", userID),
				"instance-id": fmt.Sprintf("%d", instanceID),
				"managed-by":  "clawreef",
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: storageSize,
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              storageClass,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: hostPath,
					Type: func() *corev1.HostPathType {
						t := corev1.HostPathDirectoryOrCreate
						return &t
					}(),
				},
			},
			ClaimRef: &corev1.ObjectReference{
				Kind:            "PersistentVolumeClaim",
				APIVersion:      "v1",
				Namespace:       namespace,
				Name:            pvcName,
				UID:             pvc.UID,
				ResourceVersion: pvc.ResourceVersion,
			},
		},
	}

	_, err = s.client.Clientset.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// PV was created by another process, wait for it to bind
			fmt.Printf("PV %s was created by another process, waiting for binding\n", pvName)
			time.Sleep(3 * time.Second)
			pvc, err := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to get PVC after PV creation: %w", err)
			}
			return pvc, nil
		}
		return nil, fmt.Errorf("failed to create PV %s: %w", pvName, err)
	}

	fmt.Printf("PV %s created successfully, waiting for binding\n", pvName)

	// Wait for PVC to be bound
	time.Sleep(3 * time.Second)
	pvc, err = s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get PVC after PV creation: %w", err)
	}

	if pvc.Status.Phase != corev1.ClaimBound {
		return nil, fmt.Errorf("PVC %s is still not bound after PV creation, status: %s", pvcName, pvc.Status.Phase)
	}

	fmt.Printf("PVC %s successfully bound to PV %s\n", pvcName, pvName)
	return pvc, nil
}

// GetPVC gets a PVC by user ID and instance ID
func (s *PVCService) GetPVC(ctx context.Context, userID, instanceID int) (*corev1.PersistentVolumeClaim, error) {
	if s.client == nil {
		return nil, fmt.Errorf("k8s client not initialized")
	}

	pvcName := s.client.GetPVCName(instanceID)
	namespace := s.client.GetNamespace(userID)

	pvc, err := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get PVC %s: %w", pvcName, err)
	}

	return pvc, nil
}

// DeletePVC deletes a PVC and associated PV
func (s *PVCService) DeletePVC(ctx context.Context, userID, instanceID int) error {
	if s.client == nil {
		return fmt.Errorf("k8s client not initialized")
	}

	pvcName := s.client.GetPVCName(instanceID)
	namespace := s.client.GetNamespace(userID)
	pvName := fmt.Sprintf("clawreef-pv-user-%d-instance-%d", userID, instanceID)

	// Get PVC first to find the associated PV
	pvc, err := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// PVC doesn't exist, still try to delete the PV directly
			fmt.Printf("PVC %s not found, trying to delete PV %s directly\n", pvcName, pvName)
			deleteErr := s.client.Clientset.CoreV1().PersistentVolumes().Delete(ctx, pvName, metav1.DeleteOptions{})
			if deleteErr != nil && !errors.IsNotFound(deleteErr) {
				fmt.Printf("Warning: failed to delete PV %s: %v\n", pvName, deleteErr)
			}
			return nil
		}
		return fmt.Errorf("failed to get PVC %s: %w", pvcName, err)
	}

	// Store the PV name before deleting PVC
	boundPVName := pvc.Spec.VolumeName

	// Delete the PVC first
	err = s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, pvcName, metav1.DeleteOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// PVC already deleted, continue to delete PV
			fmt.Printf("PVC %s already deleted\n", pvcName)
		} else {
			return fmt.Errorf("failed to delete PVC %s: %w", pvcName, err)
		}
	} else {
		fmt.Printf("PVC %s deleted successfully\n", pvcName)
	}

	// Delete the manually created PV
	// First try the predictable name
	err = s.client.Clientset.CoreV1().PersistentVolumes().Delete(ctx, pvName, metav1.DeleteOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			fmt.Printf("PV %s (predictable name) not found\n", pvName)
		} else {
			fmt.Printf("Warning: failed to delete PV %s: %v\n", pvName, err)
		}
	} else {
		fmt.Printf("PV %s deleted successfully\n", pvName)
	}

	// Also try to delete the bound PV if it exists and is different
	if boundPVName != "" && boundPVName != pvName {
		err = s.client.Clientset.CoreV1().PersistentVolumes().Delete(ctx, boundPVName, metav1.DeleteOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				fmt.Printf("PV %s (bound) not found or already deleted\n", boundPVName)
			} else {
				fmt.Printf("Warning: failed to delete bound PV %s: %v\n", boundPVName, err)
			}
		} else {
			fmt.Printf("Bound PV %s deleted successfully\n", boundPVName)
		}
	}

	return nil
}

// DeleteTeamSharedPVC deletes a Team shared PVC and the predictable hostPath PV
// used by the single-node manual storage fallback. It does not delete an
// arbitrary bound PV because that may belong to a real RWX provisioner.
func (s *PVCService) DeleteTeamSharedPVC(ctx context.Context, userID, teamID int) error {
	if s.client == nil {
		return fmt.Errorf("k8s client not initialized")
	}

	pvcName := s.client.GetTeamSharedPVCName(teamID)
	namespace := s.client.GetNamespace(userID)
	pvName := fmt.Sprintf("clawreef-pv-user-%d-team-%d-shared", userID, teamID)

	if err := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, pvcName, metav1.DeleteOptions{}); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to delete Team shared PVC %s/%s: %w", namespace, pvcName, err)
		}
	}
	if err := s.client.Clientset.CoreV1().PersistentVolumes().Delete(ctx, pvName, metav1.DeleteOptions{}); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to delete Team shared PV %s: %w", pvName, err)
		}
	}
	return nil
}

// PVCExists checks if a PVC exists
func (s *PVCService) PVCExists(ctx context.Context, userID, instanceID int) (bool, error) {
	_, err := s.GetPVC(ctx, userID, instanceID)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
