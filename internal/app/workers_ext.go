package app

import (
	"context"
	"fmt"
	"log"
	"strings"
)

func (a *App) dispatchPendingAlerts(ctx context.Context) error {
	alerts, err := a.store.ListPendingAlerts(ctx, 200)
	if err != nil {
		return err
	}
	var firstErr error
	for _, al := range alerts {
		message := fmt.Sprintf("[%s] %s", strings.ToUpper(al.Type), al.Message)
		sendErr := error(nil)
		if al.Channel == "email" {
			to := make([]string, 0, 4)
			for _, p := range strings.Split(a.cfg.SMTPTo, ",") {
				if v := strings.TrimSpace(p); v != "" {
					to = append(to, v)
				}
			}
			sendErr = a.notifier.SendMail("Neutrino Alert", message, to)
		} else {
			sendErr = a.notifier.SendTelegramAdmins(message)
		}
		if sendErr != nil {
			log.Printf("alert send failed id=%d channel=%s err=%v", al.ID, al.Channel, sendErr)
			if firstErr == nil {
				firstErr = sendErr
			}
			continue
		}
		if err := a.store.MarkAlertSent(ctx, al.ID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			log.Printf("mark alert sent failed id=%d err=%v", al.ID, err)
		}
	}
	return firstErr
}
