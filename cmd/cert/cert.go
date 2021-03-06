package cert

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"

	cmdutil "github.com/qqbuby/kconfig/cmd/util"
	cmdutilpkix "github.com/qqbuby/kconfig/cmd/util/pkix"
)

const (
	flagUserName   = "username"
	flagGroups     = "group"
	flagExpiration = "expiration"
	flagOutput     = "output"

	expirationSeconds = 60 * 60 * 24 * 365 // one year in seconds
)

type CertOptions struct {
	clientSet    clientset.Interface
	configAccess clientcmd.ConfigAccess
	csrName      string
	userName     string
	groups       []string
	output       string
}

func NewCmdCert(configFlags *genericclioptions.ConfigFlags) *cobra.Command {
	o := CertOptions{
		configAccess: clientcmd.NewDefaultPathOptions(),
	}

	cmd := &cobra.Command{
		Use:   "cert",
		Short: "Create kubeconfig file with a specified certificate resources.",
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(configFlags))
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.Run())
		},
	}

	cmd.Flags().StringVarP(&o.userName, flagUserName, "u", "", "user name")
	cmd.MarkFlagRequired(flagUserName)
	cmd.Flags().StringArrayVarP(&o.groups, flagGroups, "g", nil, "group name")
	cmd.MarkFlagRequired(flagGroups)
	cmd.Flags().StringVarP(&o.output, flagOutput, "o", "", "output file - default stdout")

	return cmd
}

func (o *CertOptions) Complete(configFlags *genericclioptions.ConfigFlags) error {
	o.csrName = o.userName + ":" + strings.Join(o.groups, ":")

	config, err := configFlags.ToRESTConfig()
	if err != nil {
		return err
	}
	o.clientSet, err = clientset.NewForConfig(config)
	if err != nil {
		return err
	}
	return nil
}

func (o *CertOptions) Validate() error {
	return nil
}

func (o *CertOptions) Run() error {
	_, err := o.getCertificateSigningRequest()
	if err == nil {
		err := o.deleteCertificatesV1CertificateSigningRequest()
		if err != nil {
			return err
		}
	}

	key, request, err := o.createCertificateRequest()
	if err != nil {
		return err
	}
	csr, err := o.createCertificatesV1CertificateSigningRequest(request)
	if err != nil {
		return err
	}

	csr.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{
		{
			Type:    certificatesv1.CertificateApproved,
			Status:  corev1.ConditionTrue,
			Message: "This CSR was approved by kconfig cert approve.",
			Reason:  "KonfigCertApprove",
		},
	}

	_, err = o.clientSet.CertificatesV1().
		CertificateSigningRequests().
		UpdateApproval(context.TODO(), o.csrName, csr, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	klog.V(2).Infof("wait csr:\"%s\" to be approved.", o.csrName)
	for {
		csr, err = o.getCertificateSigningRequest()
		if err != nil {
			return err
		}
		if csr.Status.Certificate != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	startingConfig, err := o.configAccess.GetStartingConfig()
	if err != nil {
		return err
	}

	ctx := startingConfig.Contexts[startingConfig.CurrentContext]
	kubeconfig := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			ctx.Cluster: startingConfig.Clusters[ctx.Cluster],
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			o.userName: {
				ClientKeyData:         key,
				ClientCertificateData: csr.Status.Certificate,
			},
		},
		Contexts: map[string]*clientcmdapi.Context{
			o.userName + "@" + ctx.Cluster: {
				Cluster:   ctx.Cluster,
				AuthInfo:  o.userName,
				Namespace: "default",
			},
		},
		CurrentContext: o.userName + "@" + ctx.Cluster,
	}

	content, err := clientcmd.Write(kubeconfig)
	if err != nil {
		return err
	}

	if len(o.output) != 0 {
		err := os.WriteFile(o.output, content, 0644)
		if err != nil {
			return err
		}
	} else {
		fmt.Fprint(os.Stdout, string(content))
	}

	klog.V(2).Infof("delete csr `%s`.", o.csrName)
	err = o.deleteCertificatesV1CertificateSigningRequest()
	if err != nil {
		return err
	}

	return nil
}

func (o *CertOptions) deleteCertificatesV1CertificateSigningRequest() error {
	gracePeriodSeconds := int64(0)
	err := o.clientSet.CertificatesV1().
		CertificateSigningRequests().
		Delete(context.TODO(), o.csrName, metav1.DeleteOptions{
			GracePeriodSeconds: &gracePeriodSeconds,
		})

	return err
}

func (o *CertOptions) createCertificatesV1CertificateSigningRequest(request []byte) (*certificatesv1.CertificateSigningRequest, error) {
	csr, err := o.clientSet.
		CertificatesV1().
		CertificateSigningRequests().
		Create(context.TODO(), &certificatesv1.CertificateSigningRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name: o.csrName,
				Annotations: map[string]string{
					"creator": "kconfig.local.io",
				},
			},
			Spec: certificatesv1.CertificateSigningRequestSpec{
				Username: o.userName,
				Groups:   o.groups,
				Usages: []certificatesv1.KeyUsage{
					certificatesv1.UsageClientAuth,
				},
				Request: request,

				SignerName: "kubernetes.io/kube-apiserver-client",
			},
		}, metav1.CreateOptions{})

	return csr, err
}

func (o *CertOptions) getCertificateSigningRequest() (*certificatesv1.CertificateSigningRequest, error) {
	csr, err := o.clientSet.CertificatesV1().
		CertificateSigningRequests().
		Get(context.TODO(), o.csrName, metav1.GetOptions{})
	return csr, err
}

func (o *CertOptions) createCertificateRequest() (keyPem []byte, csrPem []byte, err error) {
	key, csr, err := cmdutilpkix.CreateDefaultCertificateRequest(o.userName, o.groups, nil)
	if err != nil {
		return nil, nil, err
	}

	keyPem, err = cmdutilpkix.PemPkcs8PKey(key)
	if err != nil {
		return nil, nil, err
	}

	csrPem, err = cmdutilpkix.PemCertificateRequest(csr)
	if err != nil {
		return nil, nil, err
	}

	return keyPem, csrPem, nil
}
