apiVersion: v1
kind: Service
metadata:
  name: virt-platform-autopilot-status
  namespace: {{ .HCO.GetNamespace | default "openshift-cnv" }}
  labels:
    app: virt-platform-autopilot
  annotations:
    service.beta.openshift.io/serving-cert-secret-name: virt-platform-autopilot-status-tls
spec:
  selector:
    app: virt-platform-autopilot
    control-plane: controller-manager
  ports:
    - name: https-status
      port: 8444
      targetPort: 8444
      protocol: TCP
