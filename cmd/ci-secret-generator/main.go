package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/getlantern/deepcopy"
	"github.com/openshift/ci-tools/pkg/bitwarden"
	"github.com/sirupsen/logrus"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/test-infra/prow/logrusutil"
)

type options struct {
	logLevel       string
	configPath     string
	bwUser         string
	dryRun         bool
	bwPasswordPath string
	maxConcurrency int

	config     []bitWardenItem
	bwPassword string
}

type bitWardenItem struct {
	ItemName    string              `json:"item_name"`
	Fields      []fieldGenerator    `json:"fields,omitempty"`
	Attachments []fieldGenerator    `json:"attachments,omitempty"`
	Password    string              `json:"password,omitempty"`
	Notes       string              `json:"notes"`
	Params      map[string][]string `json:"params,omitempty"`
}

type fieldGenerator struct {
	Name string `json:"name,omitempty"`
	Cmd  string `json:"cmd,omitempty"`
}

func parseOptions() options {
	var o options
	flag.CommandLine.BoolVar(&o.dryRun, "dry-run", true, "Whether to actually create the secrets with bw command")
	flag.CommandLine.StringVar(&o.configPath, "config", "", "Path to the config file to use for this tool.")
	flag.CommandLine.StringVar(&o.bwUser, "bw-user", "", "Username to access BitWarden.")
	flag.CommandLine.StringVar(&o.bwPasswordPath, "bw-password-path", "", "Path to a password file to access BitWarden.")
	flag.CommandLine.StringVar(&o.logLevel, "log-level", "info", fmt.Sprintf("Log level is one of %v.", logrus.AllLevels))
	flag.CommandLine.IntVar(&o.maxConcurrency, "concurrency", 1, "Maximum number of concurrent in-flight goroutines to BitWarden.")
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Errorf("cannot parse args: %q", os.Args[1:])
	}
	return o
}

func (o *options) validateOptions() error {
	level, err := logrus.ParseLevel(o.logLevel)
	if err != nil {
		return fmt.Errorf("invalid log level specified: %w", err)
	}
	logrus.SetLevel(level)
	if o.bwUser == "" {
		return fmt.Errorf("--bw-user is empty")
	}
	if o.bwPasswordPath == "" {
		return fmt.Errorf("--bw-password-path is empty")
	}
	if o.configPath == "" {
		return fmt.Errorf("--config is empty")
	}
	return nil
}

func (o *options) completeOptions(secrets sets.String) error {
	bytes, err := ioutil.ReadFile(o.bwPasswordPath)
	if err != nil {
		return err
	}
	o.bwPassword = strings.TrimSpace(string(bytes))
	secrets.Insert(o.bwPassword)

	bytes, err = ioutil.ReadFile(o.configPath)
	if err != nil {
		return err
	}

	err = yaml.Unmarshal(bytes, &o.config)
	if err != nil {
		return err
	}
	return o.validateCompletedOptions()
}

func cmdEmptyErr(itemIndex, entryIndex int, entry string) error {
	return fmt.Errorf("config[%d].%s[%d]: empty field not allowed for cmd if name is specified", itemIndex, entry, entryIndex)
}

func (o *options) validateCompletedOptions() error {
	if o.bwPassword == "" {
		return fmt.Errorf("--bw-password-file was empty")
	}

	for i, bwItem := range o.config {
		if bwItem.ItemName == "" {
			return fmt.Errorf("config[%d].itemName: empty key is not allowed", i)
		}

		for fieldIndex, field := range bwItem.Fields {
			if field.Name != "" && field.Cmd == "" {
				return cmdEmptyErr(i, fieldIndex, "fields")
			}
		}
		for attachmentIndex, attachment := range bwItem.Fields {
			if attachment.Name != "" && attachment.Cmd == "" {
				return cmdEmptyErr(i, attachmentIndex, "attachments")
			}
		}
		for paramName, params := range bwItem.Params {
			if len(params) == 0 {
				return fmt.Errorf("at least one argument required for param: %s, itemName: %s", paramName, bwItem.ItemName)
			}
		}
	}
	return nil
}

func executeCommand(command string) ([]byte, error) {
	out, err := exec.Command("bash", "-c", command).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("command %q failed, output- %s : %w", command, string(out), err)
	}
	return out, nil
}

func replaceParameter(paramName, param, template string) string {
	return strings.ReplaceAll(template, fmt.Sprintf("$(%s)", paramName), param)
}

func processBwParameters(bwItems []bitWardenItem) ([]bitWardenItem, error) {
	var errs []error
	processedBwItems := []bitWardenItem{}
	for _, bwItemWithParams := range bwItems {
		hasErrors := false
		bwItemsProcessingHolder := []bitWardenItem{bwItemWithParams}
		for paramName, params := range bwItemWithParams.Params {
			bwItemsProcessed := []bitWardenItem{}
			for _, qItem := range bwItemsProcessingHolder {
				for _, param := range params {
					argItem := bitWardenItem{}
					err := deepcopy.Copy(&argItem, &qItem)
					if err != nil {
						errs = append(errs, fmt.Errorf("error copying bitWardenItem %v: %w", bwItemWithParams, err))
					}
					argItem.ItemName = replaceParameter(paramName, param, argItem.ItemName)
					for i, field := range argItem.Fields {
						argItem.Fields[i].Name = replaceParameter(paramName, param, field.Name)
						argItem.Fields[i].Cmd = replaceParameter(paramName, param, field.Cmd)
					}
					for i, attachment := range argItem.Attachments {
						argItem.Attachments[i].Name = replaceParameter(paramName, param, attachment.Name)
						argItem.Attachments[i].Cmd = replaceParameter(paramName, param, attachment.Cmd)
					}
					argItem.Password = replaceParameter(paramName, param, argItem.Password)
					argItem.Notes = replaceParameter(paramName, param, argItem.Notes)
					bwItemsProcessed = append(bwItemsProcessed, argItem)
				}
			}
			bwItemsProcessingHolder = bwItemsProcessed
		}
		if !hasErrors {
			processedBwItems = append(processedBwItems, bwItemsProcessingHolder...)
		}
	}
	return processedBwItems, utilerrors.NewAggregate(errs)
}

func updateSecrets(bwItems []bitWardenItem, bwClient bitwarden.Client) error {
	var errs []error
	processedBwItems, err := processBwParameters(bwItems)
	if err != nil {
		errs = append(errs, fmt.Errorf("error parsing parameters: %w", err))
	}
	for _, bwItem := range processedBwItems {
		logger := logrus.WithField("item", bwItem.ItemName)
		for _, field := range bwItem.Fields {
			logger = logger.WithFields(logrus.Fields{
				"field":   field.Name,
				"command": field.Cmd,
			})
			logger.Info("processing field")
			out, err := executeCommand(field.Cmd)
			if err != nil {
				logrus.WithError(err).Errorf("%s failed to generate field", field.Cmd)
				errs = append(errs, fmt.Errorf("bwItem.ItemName: %s, bwItem.FieldName: %s, %s failed: %w", bwItem.ItemName, field.Name, field.Cmd, err))
				continue
			}
			if err := bwClient.SetFieldOnItem(bwItem.ItemName, field.Name, out); err != nil {
				errs = append(errs, fmt.Errorf("bwItem.ItemName: %s, bwItem.FieldName: %s, failed to upload field: %w", bwItem.ItemName, field.Name, err))
				logrus.WithError(err).Error("failed to upload field")
				continue
			}
		}
		for _, attachment := range bwItem.Attachments {
			logger = logger.WithFields(logrus.Fields{
				"attachment": attachment.Name,
				"command":    attachment.Cmd,
			})
			logger.Info("processing attachment")
			out, err := executeCommand(attachment.Cmd)
			if err != nil {
				errs = append(errs, fmt.Errorf("bwItem.ItemName: %s, bwItem.AttachmentName: %s, %s failed: %w", bwItem.ItemName, attachment.Name, attachment.Cmd, err))
				logrus.WithError(err).Errorf("%s: failed to generate attachment", attachment.Cmd)
				continue
			}
			if err := bwClient.SetAttachmentOnItem(bwItem.ItemName, attachment.Name, out); err != nil {
				errs = append(errs, fmt.Errorf("bwItem.ItemName: %s, bwItem.AttachmentName: %s, failed to upload attachment: %w", bwItem.ItemName, attachment.Name, err))
				logrus.WithError(err).Error("failed to upload attachment")
				continue
			}
		}
		if bwItem.Password != "" {
			logger = logger.WithFields(logrus.Fields{
				"password": bwItem.Password,
			})
			logger.Info("processing password")
			out, err := executeCommand(bwItem.Password)
			if err != nil {
				errs = append(errs, fmt.Errorf("bwItem.ItemName: %s, bwItem.Password:, %s failed: %w", bwItem.ItemName, bwItem.Password, err))
				logrus.WithError(err).Errorf("%s :failed to generate password", bwItem.Password)
			} else {
				if err := bwClient.SetPassword(bwItem.ItemName, out); err != nil {
					errs = append(errs, fmt.Errorf("bwItem.ItemName: %s, bwItem.Password:, failed to upload password: %w", bwItem.ItemName, err))
					logrus.WithError(err).Error("failed to upload password")
				}
			}
		}

		// Adding the notes not empty check here since we dont want to overwrite any notes that might already be present
		// If notes have to be deleted, it would have to be a manual operation where the user goes to the bw web UI and removes
		// the notes
		if bwItem.Notes != "" {
			logger = logger.WithFields(logrus.Fields{
				"notes": bwItem.Notes,
			})
			logger.Info("adding notes")
			if err := bwClient.UpdateNotesOnItem(bwItem.ItemName, bwItem.Notes); err != nil {
				errs = append(errs, fmt.Errorf("bwItem.ItemName: %s,  failed to update notes: %w", bwItem.ItemName, err))
				logrus.WithError(err).Error("failed to update notes")
			}
		}
	}
	return utilerrors.NewAggregate(errs)
}

func main() {
	// CLI tool which does the secret generation and uploading to bitwarden
	o := parseOptions()
	secrets := sets.NewString()
	logrus.SetFormatter(logrusutil.NewCensoringFormatter(logrus.StandardLogger().Formatter, func() sets.String {
		return secrets
	}))
	if err := o.validateOptions(); err != nil {
		logrus.WithError(err).Fatal("invalid arguments.")
	}
	if err := o.completeOptions(secrets); err != nil {
		logrus.WithError(err).Fatal("failed to complete options.")
	}
	var client bitwarden.Client
	if o.dryRun {
		tmpFile, err := ioutil.TempFile("", "ci-secret-generator")
		if err != nil {
			logrus.WithError(err).Fatal("failed to create tempfile")
		}
		client, err = bitwarden.NewDryRunClient(tmpFile)
		if err != nil {
			logrus.WithError(err).Fatal("failed to create dryRun client")
		}
		logrus.Infof("Dry-Run enabled, writing secrets to %s", tmpFile.Name())
	} else {
		var err error
		client, err = bitwarden.NewClient(o.bwUser, o.bwPassword, func(s string) {
			secrets.Insert(s)
		})
		if err != nil {
			logrus.WithError(err).Fatal("failed to get Bitwarden client.")
		}
	}
	logrus.RegisterExitHandler(func() {
		if _, err := client.Logout(); err != nil {
			logrus.WithError(err).Fatal("failed to logout.")
		}
	})
	defer logrus.Exit(0)

	// Upload the output to bitwarden
	if err := updateSecrets(o.config, client); err != nil {
		logrus.WithError(err).Fatalf("Failed to update secrets.")
	}
	logrus.Info("Updated secrets.")
}